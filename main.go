package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/google/go-github/v65/github"
	"golang.org/x/oauth2"
)

func main() {
	newCommitHashFlag := flag.String("c", "", "8-character commit hash")
	owner := flag.String("o", "", "GitHub owner")
	repo := flag.String("r", "", "GitHub repository")
	env := flag.String("e", "", "Environment (dev or prod)")
	yamlFile := flag.String("f", "", "YAML File")
	yamlTag := flag.String("t", "", "YAML Tag")

	master := flag.String("m", "master", "Name of main branch")

	flag.Parse()

	if *env != "dev" && *env != "prod" {
		log.Fatal("Environment (-e) must be 'dev' or 'prod'")
	}

	if *owner == "" {
		log.Fatal("Organization (-o) is required")
	}

	if *repo == "" {
		log.Fatal("Repository (-r) is required")
	}

	ghToken, err := runCommand("gh", "auth", "status", "--show-token")
	if err != nil {
		log.Fatalf("Failed to get token from gh cli client: %v", err)
	}

	lines := strings.Split(ghToken, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Token:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ghToken = parts[2]
				break
			}
		}
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: ghToken},
	)

	ctx := context.Background()
	gh := github.NewClient(oauth2.NewClient(ctx, ts))

	newCommitHash := *newCommitHashFlag
	if newCommitHash == "" {
		newCommitHash, err = extractNewCommitHash(ctx, gh, *master, *owner, *repo)
		if err != nil {
			log.Fatalf("Failed to extract newest master: %v", err)
		}
	}

	oldCommitHash, err := extractOldCommitHash(*yamlFile, *master, *yamlTag)
	if err != nil {
		log.Fatalf("Could not extract the release tag from %s: %v", *yamlFile, err)
	}

	branchName := fmt.Sprintf("%s_%s_%s", *repo, *env, newCommitHash)
	if err := createOrCheckoutBranch(branchName); err != nil {
		log.Fatalf("Failed to create or checkout branch: %v", err)
	}

	// Update the YAML file
	if err := updateYamlFile(*yamlFile, *master, *yamlTag, oldCommitHash, newCommitHash); err != nil {
		log.Fatalf("Failed to update YAML file: %v", err)
	}

	// Commit the changes
	if err := gitCommit(*yamlFile, *env, *repo, newCommitHash); err != nil {
		log.Fatalf("Failed to commit changes: %v", err)
	}

	// Push the branch
	if err := gitPush(branchName); err != nil {
		log.Fatalf("Failed to push branch: %v", err)
	}

	title := fmt.Sprintf("%s %s %s", *repo, strings.ToUpper(*env), newCommitHash)
	description := fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s", *owner, *repo, oldCommitHash, newCommitHash)
	pullRequestURL, err := createPullRequest(ctx, gh, *owner, branchName, *master, title, description)
	if err != nil {
		log.Fatalf("Failed to create pull request: %v", err)
	}

	fmt.Printf("Pull request created successfully\nURL: %s\nTitle: %s\nDescription:\n%s\n", pullRequestURL, title, description)
}

func extractNewCommitHash(ctx context.Context, gh *github.Client, master, owner, repository string) (string, error) {
	ref, _, err := gh.Git.GetRef(ctx, owner, repository, "refs/heads/"+master)
	if err != nil {
		return "", fmt.Errorf("error getting ref: %w", err)
	}

	return ref.GetObject().GetSHA()[:8], nil
}

func extractOldCommitHash(filePath, master, yamlTag string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	re := regexp.MustCompile(master + `_(\w{8})`)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, yamlTag) {
			match := re.FindStringSubmatch(line)
			if len(match) > 1 {
				return match[1], nil
			}
		}
	}

	return "", fmt.Errorf("commit hash not found")
}

func createOrCheckoutBranch(branch string) error {
	// Check if the branch exists
	if _, err := runCommand("git", "rev-parse", "--verify", branch); err == nil {
		if _, err := runCommand("git", "checkout", branch); err != nil {
			return fmt.Errorf("run git checkout %s: %w", branch, err)
		}

		return nil
	}

	if _, err := runCommand("git", "checkout", "-b", branch); err != nil {
		return fmt.Errorf("run git checkout -b %s: %w", branch, err)
	}

	return nil
}

func updateYamlFile(filePath, master, yamlTag, oldCommitHash, newCommitHash string) error {
	input, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	output := strings.Replace(string(input),
		fmt.Sprintf(`%s: "%s_%s"`, yamlTag, master, oldCommitHash),
		fmt.Sprintf(`%s: "%s_%s"`, yamlTag, master, newCommitHash),
		1)

	if err := os.WriteFile(filePath, []byte(output), 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func gitCommit(filePath, env, repo, commitHash string) error {
	if _, err := runCommand("git", "add", filePath); err != nil {
		return fmt.Errorf("run git add: %w", err)
	}

	commitMessage := fmt.Sprintf("%s %s %s", repo, strings.ToUpper(env), commitHash)
	if _, err := runCommand("git", "commit", "-m", commitMessage); err != nil {
		return fmt.Errorf(`run git commit -m "%s": %w`, commitMessage, err)
	}

	return nil
}

func gitPush(branch string) error {
	if _, err := runCommand("git", "push", "origin", branch); err != nil {
		return fmt.Errorf(`run push: %w`, err)
	}

	return nil
}

func createPullRequest(ctx context.Context, gh *github.Client, owner, branch, master, title, description string) (string, error) {
	pr := &github.NewPullRequest{
		Title: &title,
		Body:  &description,
		Head:  &branch,
		Base:  &master,
	}

	createdPR, _, err := gh.PullRequests.Create(ctx, owner, "devops", pr)
	if err != nil {
		return "", fmt.Errorf("create pull request: %w", err)
	}

	return createdPR.GetHTMLURL(), nil
}

func runCommand(name string, args ...string) (string, error) {
	var out bytes.Buffer

	cmd := exec.Command(name, args...)
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("run: %w", err)
	}

	return out.String(), nil
}
