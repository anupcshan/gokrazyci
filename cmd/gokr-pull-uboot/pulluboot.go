package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/google/go-github/v35/github"
)

// getUpstreamCommit returns the SHA of the most recent
// github.com/u-boot/u-boot git commit.
func getUpstreamCommit(ctx context.Context, client *github.Client) (*github.RepositoryCommit, error) {
	commits, _, err := client.Repositories.ListCommits(ctx, "u-boot", "u-boot", &github.CommitsListOptions{
		ListOptions: github.ListOptions{
			Page:    1,
			PerPage: 1,
		},
	})
	if err != nil {
		return nil, err
	}

	log.Printf("picked %s as most recent upstream u-boot commit", commits[0].GetSHA())

	log.Println(commits[0].GetCommit().GetAuthor().GetDate())

	return commits[0], nil
}

func updateFirmware(ctx context.Context, client *github.Client, owner, repo string) error {
	upstreamCommit, err := getUpstreamCommit(ctx, client)
	if err != nil {
		return err
	}

	upstreamSHA := upstreamCommit.GetSHA()

	lastRef, _, err := client.Git.GetRef(ctx, owner, repo, "heads/master")
	if err != nil {
		return err
	}

	lastCommit, _, err := client.Git.GetCommit(ctx, owner, repo, *lastRef.Object.SHA)
	if err != nil {
		return err
	}

	log.Printf("lastCommit = %+v", lastCommit)

	baseTree, _, err := client.Git.GetTree(ctx, owner, repo, *lastCommit.SHA, true)
	if err != nil {
		return err
	}
	log.Printf("baseTree = %+v", baseTree)

	var (
		updaterSHA  string
		updaterPath = "cmd/gokr-build-uboot/build.go"
	)
	for _, entry := range baseTree.Entries {
		if *entry.Path == updaterPath {
			updaterSHA = *entry.SHA
			break
		}
	}

	if updaterSHA == "" {
		return fmt.Errorf("%s not found in %s/%s", updaterPath, owner, repo)
	}

	updaterBlob, _, err := client.Git.GetBlob(ctx, owner, repo, updaterSHA)
	if err != nil {
		return err
	}

	updaterContent, err := base64.StdEncoding.DecodeString(*updaterBlob.Content)
	if err != nil {
		return err
	}

	ubootRevRe := regexp.MustCompile(`const ubootRev = "([0-9a-f]+)"`)
	matches := ubootRevRe.FindStringSubmatch(string(updaterContent))
	if matches == nil {
		return fmt.Errorf("regexp %v resulted in no matches", ubootRevRe)
	}
	if matches[1] == upstreamSHA {
		log.Printf("already at latest commit")
		return nil
	}

	newContent := ubootRevRe.ReplaceAllLiteral(updaterContent,
		[]byte(fmt.Sprintf(`const ubootRev = "%s"`, upstreamSHA)))

	ubootTSRe := regexp.MustCompile(`const ubootTS = ([0-9]+)`)
	newContent = ubootTSRe.ReplaceAllLiteral(newContent, []byte(fmt.Sprintf(`const ubootTS = %d`, upstreamCommit.GetCommit().GetAuthor().GetDate().Unix())))

	entries := []*github.TreeEntry{
		{
			Path:    github.String(updaterPath),
			Mode:    github.String("100644"),
			Type:    github.String("blob"),
			Content: github.String(string(newContent)),
		},
	}

	newTree, _, err := client.Git.CreateTree(ctx, owner, repo, *baseTree.SHA, entries)
	if err != nil {
		return err
	}
	log.Printf("newTree = %+v", newTree)

	newCommit, _, err := client.Git.CreateCommit(ctx, owner, repo, &github.Commit{
		Message: github.String("auto-update to https://github.com/u-boot/u-boot/commit/" + upstreamSHA),
		Tree:    newTree,
		Parents: []*github.Commit{lastCommit},
	})
	if err != nil {
		return err
	}
	log.Printf("newCommit = %+v", newCommit)

	newRef, _, err := client.Git.CreateRef(ctx, owner, repo, &github.Reference{
		Ref: github.String("refs/heads/pull-" + upstreamSHA),
		Object: &github.GitObject{
			SHA: newCommit.SHA,
		},
	})
	if err != nil {
		return err
	}
	log.Printf("newRef = %+v", newRef)

	pr, _, err := client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.String("auto-update to " + upstreamSHA),
		Head:  github.String("pull-" + upstreamSHA),
		Base:  github.String("master"),
	})
	if err != nil {
		return err
	}

	log.Printf("pr = %+v", pr)

	return nil
}

var githubUser, authToken, slug string

func loadEnv() error {
	githubUser = os.Getenv("GH_USER")
	authToken = os.Getenv("GH_AUTH_TOKEN")
	slug = os.Getenv("GITHUB_REPOSITORY")

	if githubUser == "" {
		return fmt.Errorf("Empty Github user")
	}
	if authToken == "" {
		return fmt.Errorf("Empty auth token")
	}
	if slug == "" {
		return fmt.Errorf("Empty slug")
	}

	return nil
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if err := loadEnv(); err != nil {
		log.Fatal(err)
	}

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	ctx := context.Background()

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: githubUser,
			Password: authToken,
		},
	})

	if err := updateFirmware(ctx, client, parts[0], parts[1]); err != nil {
		log.Fatal(err)
	}
}
