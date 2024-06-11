//
// Copyright 2024 Stacklok, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/google/go-github/v60/github"
	"github.com/stacklok/frizbee/pkg/replacer"
	"github.com/stacklok/frizbee/pkg/utils/config"
	"golang.org/x/oauth2"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type FrizbeeAction struct {
	client            *github.Client
	RepoOwner         string
	RepoName          string
	ActionsPath       string
	DockerfilesPath   string
	KubernetesPath    string
	DockerComposePath string
	OpenPR            bool
	FailOnUnpinned    bool
	ActionsReplacer   *replacer.Replacer
	ImagesReplacer    *replacer.Replacer
}

// ErrUnpinnedFound is the error returned when unpinned actions or container images are found
var ErrUnpinnedFound = errors.New("frizbee found unpinned actions or container images")

func main() {
	ctx := context.Background()
	// Initialize the frizbee action
	frizbeeAction, err := initAction(ctx)
	if err != nil {
		log.Fatalf("Error initializing action: %v", err)
	}

	// Run the frizbee action
	err = frizbeeAction.Run(ctx)
	if err != nil {
		if errors.Is(err, ErrUnpinnedFound) {
			log.Printf("Unpinned actions or container images found. Check the Frizbee Action logs for more information.")
			os.Exit(1)
		}
		log.Fatalf("Error running action: %v", err)
	}
}

// initAction initializes the frizbee action - reads the environment variables, creates the GitHub client, etc.
func initAction(ctx context.Context) (*FrizbeeAction, error) {
	// Get the GitHub token from the environment
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN environment variable is not set")
	}

	// Create a new GitHub client
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)

	// Get the GITHUB_REPOSITORY_OWNER
	repoOwner := os.Getenv("GITHUB_REPOSITORY_OWNER")
	if repoOwner == "" {
		return nil, fmt.Errorf("GITHUB_REPOSITORY_OWNER environment variable is not set")
	}

	// Split the GITHUB_REPOSITORY environment variable to get repo name
	repoFullName := os.Getenv("GITHUB_REPOSITORY")
	if repoFullName == "" {
		return nil, fmt.Errorf("GITHUB_REPOSITORY environment variable is not set")
	}

	// Read the action settings from the environment and create the new frizbee replacers for actions and images
	return &FrizbeeAction{
		client:            github.NewClient(tc),
		RepoOwner:         repoOwner,
		RepoName:          strings.TrimPrefix(repoFullName, repoOwner+"/"),
		ActionsPath:       os.Getenv("INPUT_ACTIONS"),
		DockerfilesPath:   os.Getenv("INPUT_DOCKERFILES"),
		KubernetesPath:    os.Getenv("INPUT_KUBERNETES"),
		DockerComposePath: os.Getenv("INPUT_DOCKER_COMPOSE"),
		OpenPR:            os.Getenv("INPUT_OPEN_PR") == "true",
		FailOnUnpinned:    os.Getenv("INPUT_FAIL_ON_UNPINNED") == "true",
		ActionsReplacer:   replacer.NewGitHubActionsReplacer(&config.Config{}).WithGitHubClientFromToken(token),
		ImagesReplacer:    replacer.NewContainerImagesReplacer(&config.Config{}),
	}, nil
}

// Run runs the frizbee action
func (fa *FrizbeeAction) Run(ctx context.Context) error {
	// Parse the workflow files
	modified, err := fa.parseWorkflowActions(ctx)
	if err != nil {
		return fmt.Errorf("failed to parse workflow files: %w", err)
	}

	// Parse all yaml/yml files referencing container images
	m, err := fa.parseImages(ctx)
	if err != nil {
		return fmt.Errorf("failed to parse image files: %w", err)
	}

	// Set the modified flag to true if any file was modified
	modified = modified || m

	// If the OpenPR flag is set, commit and push the changes and create a pull request
	if fa.OpenPR && modified {
		// TODO: use the git library to commit and push changes
		// TODO: perhaps refactor the code so instead of having 1 commit, we have separate commits for each file that
		// TODO: frizbee modified
		commitAndPushChanges()
		createPullRequest()
	}

	// Exit with ErrUnpinnedFound error if any files were modified and the action is set to fail on unpinned
	if fa.FailOnUnpinned && modified {
		return ErrUnpinnedFound
	}

	return nil
}

// parseWorkflowActions parses the GitHub Actions workflow files and updates the modified files if the OpenPR flag is set
func (fa *FrizbeeAction) parseWorkflowActions(ctx context.Context) (bool, error) {
	if fa.ActionsPath == "" {
		log.Printf("Workflow path is empty")
		return false, nil
	}

	log.Printf("Parsing workflow files in %s...", fa.ActionsPath)
	res, err := fa.ActionsReplacer.ParsePath(ctx, fa.ActionsPath)
	if err != nil {
		return false, fmt.Errorf("failed to parse workflow files in %s: %w", fa.ActionsPath, err)
	}

	return fa.processOutput(res, fa.ActionsPath)
}

// parseImages parses the Dockerfiles, Docker Compose, and Kubernetes files for container images.
// It also updates the files if the OpenPR flag is set
func (fa *FrizbeeAction) parseImages(ctx context.Context) (bool, error) {
	var modified bool
	pathsToParse := []string{fa.DockerfilesPath, fa.DockerComposePath, fa.KubernetesPath}
	for _, path := range pathsToParse {
		if path == "" {
			continue
		}
		log.Printf("Parsing files for container images in %s", path)
		res, err := fa.ImagesReplacer.ParsePath(ctx, path)
		if err != nil {
			return false, fmt.Errorf("failed to parse: %w", err)
		}
		// Process the parsing output
		m, err := fa.processOutput(res, path)
		if err != nil {
			return false, fmt.Errorf("failed to process output: %w", err)
		}
		// Set the modified flag to true if any file was modified
		modified = modified || m
	}
	return modified, nil
}

// processOutput processes the output of a replacer, prints the processed and modified files and writes the
// changes to the files
func (fa *FrizbeeAction) processOutput(res *replacer.ReplaceResult, baseDir string) (bool, error) {
	var modified bool
	bfs := osfs.New(baseDir, osfs.WithBoundOS())

	// Show the processed files
	for _, path := range res.Processed {
		log.Printf("Processed file: %s", filepath.Base(path))
	}

	// Process the modified files
	for path, content := range res.Modified {
		log.Printf("Modified file: %s", filepath.Base(path))
		log.Printf("Modified content:\n%s\n", content)
		// Overwrite the content of the file with the changes if the OpenPR flag is set
		if fa.OpenPR {
			f, err := bfs.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				return modified, fmt.Errorf("failed to open file %s: %w", filepath.Base(path), err)
			}
			defer func() {
				if err := f.Close(); err != nil {
					log.Fatalf("failed to close file %s: %v", filepath.Base(path), err) // nolint:errcheck
				}
			}()
			_, err = fmt.Fprintf(f, "%s", content)
			if err != nil {
				return modified, fmt.Errorf("failed to write to file %s: %w", filepath.Base(path), err)
			}
			// Set the modified flag to true if any file was modified
			modified = true
		}
	}
	return modified, nil
}

func runCommand(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Fatalf("Failed to run command %s %v: %v", name, args, err)
	}
}

func commitAndPushChanges() {
	// Configure git
	runCommand("git", "config", "--global", "--add", "safe.directory", "/github/workspace")
	runCommand("git", "config", "--global", "user.name", "frizbee-action[bot]")
	runCommand("git", "config", "--global", "user.email", "frizbee-action[bot]@users.noreply.github.com")

	// Get git status
	runCommand("git", "status")

	// Create a new branch
	branchName := "modify-workflows"
	runCommand("git", "checkout", "-b", branchName)

	// Add changes
	runCommand("git", "add", ".")

	// Commit changes
	runCommand("git", "commit", "-m", "frizbee: pin images and actions to commit hash")

	// Show the changes
	runCommand("git", "show")

	// Push changes
	runCommand("git", "push", "origin", branchName, "--force")
}

func createPullRequest() {
	title := "Frizbee: Pin images and actions to commit hash"
	body := "This PR pins images and actions to their commit hash"
	head := "modify-workflows"
	base := "main"
	runCommand("gh", "pr", "create", "--title", title, "--body", body, "--head", head, "--base", base)
}
