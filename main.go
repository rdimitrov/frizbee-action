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
	Silent            bool
	ActionsReplacer   *replacer.Replacer
	ImagesReplacer    *replacer.Replacer
}

func main() {
	ctx := context.Background()
	frizbeeAction, err := initAction(ctx)
	if err != nil {
		log.Fatalf("Error initializing action: %v", err)
	}

	err = frizbeeAction.Run(ctx)
	if err != nil {
		log.Fatalf("Error running action: %v", err)
	}
}

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

	// Read the action settings from the environment
	return &FrizbeeAction{
		client:            github.NewClient(tc),
		RepoOwner:         repoOwner,
		RepoName:          strings.TrimPrefix(repoFullName, repoOwner+"/"),
		ActionsPath:       os.Getenv("INPUT_ACTIONS"),
		DockerfilesPath:   os.Getenv("INPUT_DOCKERFILES"),
		KubernetesPath:    os.Getenv("INPUT_KUBERNETES"),
		DockerComposePath: os.Getenv("INPUT_DOCKER_COMPOSE"),
		OpenPR:            os.Getenv("INPUT_OPEN_PR") == "true",
		Silent:            os.Getenv("INPUT_SILENT") == "true",
		ActionsReplacer:   replacer.NewGitHubActionsReplacer(&config.Config{}).WithGitHubClientFromToken(token),
		ImagesReplacer:    replacer.NewContainerImagesReplacer(&config.Config{}),
	}, nil
}

func (fa *FrizbeeAction) Run(ctx context.Context) error {
	// Parse the workflow files
	log.Println("Parsing workflow files")
	err := fa.parseWorkflowActions(ctx)
	if err != nil {
		return fmt.Errorf("failed to parse workflow files: %w", err)
	}
	log.Println("Parsing images")

	// Parse the image files
	err = fa.parseImages(ctx)
	if err != nil {
		return fmt.Errorf("failed to parse image files: %w", err)
	}

	if fa.OpenPR {
		commitAndPushChanges()
		createPullRequest()
	}
	return nil
}

func (fa *FrizbeeAction) parseImages(ctx context.Context) error {
	pathsToParse := []string{fa.DockerfilesPath, fa.DockerComposePath, fa.KubernetesPath}
	for _, path := range pathsToParse {
		if path == "" {
			continue
		}
		log.Printf("Parsing files in %s", path)
		res, err := fa.ImagesReplacer.ParsePath(ctx, path)
		if err != nil {
			return fmt.Errorf("failed to parse: %w", err)
		}
		err = fa.processOutput(res, path)
		if err != nil {
			return fmt.Errorf("failed to process output: %w", err)
		}
	}
	return nil
}

func (fa *FrizbeeAction) parseWorkflowActions(ctx context.Context) error {
	if fa.ActionsPath == "" {
		log.Printf("No workflow files to parse")
		return nil
	}

	log.Printf("Parsing workflow files in %s", fa.ActionsPath)
	res, err := fa.ActionsReplacer.ParsePath(ctx, ".github/workflows")
	if err != nil {
		return fmt.Errorf("failed to parse workflow files: %w", err)
	}
	return fa.processOutput(res, fa.ActionsPath)
}

func (fa *FrizbeeAction) processOutput(res *replacer.ReplaceResult, baseDir string) error {
	bfs := osfs.New(baseDir, osfs.WithBoundOS())
	// Print the processed files
	for _, path := range res.Processed {
		log.Printf("Processed file: %s", filepath.Base(path))
	}
	// Print the modified files
	for path, content := range res.Modified {
		log.Printf("Modified file: %s", filepath.Base(path))
		log.Printf("Modified content:\n%s\n", content)
		if fa.OpenPR {
			f, err := bfs.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				return fmt.Errorf("failed to open file %s: %w", filepath.Base(path), err)
			}
			defer func() {
				if err := f.Close(); err != nil {
					log.Fatalf("failed to close file %s: %v", filepath.Base(path), err) // nolint:errcheck
				}
			}()
			_, err = fmt.Fprintf(f, "%s", content)
			if err != nil {
				return fmt.Errorf("failed to write to file %s: %w", filepath.Base(path), err)
			}
		}
	}
	return nil
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
	runCommand("git", "add", ".github/workflows")

	// Commit changes
	runCommand("git", "commit", "-m", "frizbee: pin images and actions to commit hash")

	// Push changes
	runCommand("git", "push", "origin", branchName, "--force")
}

func createPullRequest() {
	title := "Frizbee: Pin images and actions to commit hash"
	body := "This PR pins images and actions to the commit hash"
	head := "modify-workflows"
	base := "main"
	runCommand("gh", "pr", "create", "--title", title, "--body", body, "--head", head, "--base", base)
}
