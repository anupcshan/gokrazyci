package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anupcshan/gotool"
	"github.com/google/go-github/v35/github"
)

const (
	githubUser      = "gokrazy-bot-2"
	githubRepoOwner = "anupcshan"
	githubRepoName  = "gokrazy-odroidxu4-kernel"
	pleaseBootLabel = "please-boot"
)

func hasPleaseBoot(pr *github.PullRequest) bool {
	for _, label := range pr.Labels {
		if label.GetName() == pleaseBootLabel {
			return true
		}
	}

	return false
}

func mostRecentRelevantPR(ctx context.Context, client *github.Client) (*github.PullRequest, error) {
	prs, _, err := client.PullRequests.List(ctx, githubRepoOwner, githubRepoName, &github.PullRequestListOptions{
		State: "open",
	})
	if err != nil {
		return nil, err
	}

	for _, pr := range prs {
		if pr.GetUser().GetLogin() != githubUser {
			continue
		}
		if !hasPleaseBoot(pr) {
			continue
		}
		return pr, nil
	}

	return nil, nil
}

func fetchToDir(ctx context.Context, client *github.Client, dir string, repoOwner string, repoName string, repoHash string) error {
	commit, _, err := client.Git.GetCommit(ctx, repoOwner, repoName, repoHash)
	if err != nil {
		return err
	}

	objects, _, err := client.Git.GetTree(ctx, repoOwner, repoName, commit.Tree.GetSHA(), true)
	if err != nil {
		return err
	}

	for _, obj := range objects.Entries {
		switch obj.GetType() {
		case "tree":
			if err := os.MkdirAll(filepath.Join(dir, obj.GetPath()), 0755); err != nil {
				return err
			}
		case "blob":
			log.Printf("Fetching %s for %s", obj.GetSHA(), obj.GetPath())
			blob, _, err := client.Git.GetBlob(ctx, repoOwner, repoName, obj.GetSHA())
			if err != nil {
				return err
			}

			contents, err := base64.StdEncoding.DecodeString(blob.GetContent())
			if err != nil {
				return err
			}

			f, err := os.Create(filepath.Join(dir, obj.GetPath()))
			if err != nil {
				return err
			}

			if _, err := f.Write(contents); err != nil {
				return err
			}

			if err := f.Close(); err != nil {
				return err
			}
		}
	}

	return nil
}

func env(goroot string) []string {
	homeDir := os.Getenv("HOME")
	return []string{
		fmt.Sprintf("HOME=%s", homeDir),
		fmt.Sprintf("PATH=%s",
			strings.Join([]string{
				filepath.Join(goroot, "bin"),
				filepath.Join(homeDir, "go/bin"),
			}, ":"),
		),
	}
}

func buildPacker(goroot string, dir string) error {
	cmd := exec.Command(filepath.Join(goroot, "bin/go"), "get", "github.com/gokrazy/tools/cmd/gokr-packer")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = dir
	cmd.Env = env(goroot)

	return cmd.Run()
}

func buildBoot(goroot string, dir string, bootPath string) error {
	cmd := exec.Command(
		filepath.Join(os.Getenv("HOME"), "go/bin", "gokr-packer"),
		"-device_type=odroidhc1",
		"-hostname=odroidbake",
		"-eeprom_package=",
		"-firmware_package=",
		"-kernel_package=github.com/anupcshan/gokrazy-odroidxu4-kernel",
		fmt.Sprintf("-overwrite_boot=%s", bootPath),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = dir
	cmd.Env = env(goroot)
	cmd.Env = append(cmd.Env, "GOARCH=arm", "GOARM=7")

	return cmd.Run()
}

func testBoot(bootFile string, booteryUrl string, buildTimestamp time.Time) error {
	f, err := os.Open(bootFile)
	if err != nil {
		return err
	}
	defer f.Close()

	u, err := url.Parse(booteryUrl)
	if err != nil {
		return err
	}
	u.Path = "testboot"
	v := u.Query()
	v.Set("slug", "anupcshan/gokrazy-odroidxu4-kernel")
	v.Set("boot-newer", strconv.FormatInt(buildTimestamp.Unix()-1, 10))
	u.RawQuery = v.Encode()

	log.Println(u.String())

	req, err := http.NewRequest(http.MethodPut, u.String(), f)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(os.Stderr, resp.Body)
	return nil
}

func main() {
	authToken := flag.String("github.authtoken", "", "Github auth token for gokrazy-bot-2")
	booteryUrl := flag.String("bootery_url", "", "Bootery URL")
	pollInterval := flag.Duration("poll_interval", 5*time.Minute, "Duration between consecutive polls for new PRs")
	flag.Parse()

	if err := os.MkdirAll(os.Getenv("HOME"), 0755); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: githubUser,
			Password: *authToken,
		},
	})

	goroot, err := gotool.InstallGo()
	if err != nil {
		log.Fatal(err)
	}
	_ = goroot

	for {
		pr, err := mostRecentRelevantPR(ctx, client)
		if err != nil {
			log.Fatal(err)
		}

		if pr != nil {
			log.Println("Most recent PR:", pr.GetNumber(), pr.GetHead().GetUser().GetLogin(), pr.GetHead().GetRepo().GetName(), pr.GetHead().GetSHA())

			dir, err := os.MkdirTemp(os.Getenv("HOME"), "testboot")
			if err != nil {
				log.Fatal(err)
			}

			log.Println(dir)

			if err := fetchToDir(ctx, client, dir, pr.GetHead().GetUser().GetLogin(), pr.GetHead().GetRepo().GetName(), pr.GetHead().GetSHA()); err != nil {
				log.Fatal(err)
			}

			if err := buildPacker(goroot, dir); err != nil {
				log.Fatal(err)
			}

			f, err := os.CreateTemp(os.Getenv("HOME"), "bootfile")
			if err != nil {
				log.Fatal(err)
			}

			now := time.Now()

			if err := buildBoot(goroot, dir, f.Name()); err != nil {
				log.Fatal(err)
			}

			if err := testBoot(f.Name(), *booteryUrl, now); err != nil {
				log.Fatal(err)
			}

			log.Println("Testboot succeeded")
			_ = os.RemoveAll(dir)

			log.Println("Adding please-merge")
			_, _, err = client.Issues.AddLabelsToIssue(ctx, githubRepoOwner, githubRepoName, pr.GetNumber(), []string{"please-merge"})
			if err != nil {
				log.Fatal(err)
			}

			log.Println("Removing please-boot")
			_, err = client.Issues.RemoveLabelForIssue(ctx, githubRepoOwner, githubRepoName, pr.GetNumber(), "please-boot")
			if err != nil {
				log.Fatal(err)
			}
		}

		log.Printf("Waiting for %s before polling again", *pollInterval)
		time.Sleep(*pollInterval)
	}
}
