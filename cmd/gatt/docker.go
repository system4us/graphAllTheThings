package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDockerImage     = "system4us/gatt:latest"
	defaultDockerContainer = "gatt-server"
	defaultDockerPort      = 7788
)

func cmdDocker(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(os.Stderr, `usage: gatt docker <subcommand> [flags]

Manage the local central gatt Docker container (MCP HTTP + ONNX + embedded Qdrant).

Subcommands:
  up      Start (or recreate) the container bound to 127.0.0.1:7788
  down    Stop and remove the container (preserves gatt-data and gatt-cache volumes)
  status  Query /status from the running container
  logs    Show container logs (-f to follow)
  pull    Pull the latest Docker image`)
		if len(args) < 1 {
			return fmt.Errorf("missing docker subcommand")
		}
		return nil
	}

	switch args[0] {
	case "up":
		return cmdDockerUp(ctx, args[1:])
	case "down":
		return cmdDockerDown(ctx, args[1:])
	case "status":
		return cmdDockerStatus(ctx, args[1:])
	case "logs":
		return cmdDockerLogs(ctx, args[1:])
	case "pull":
		return cmdDockerPull(ctx, args[1:])
	default:
		return fmt.Errorf("unknown docker subcommand %q (expected: up, down, status, logs, pull)", args[0])
	}
}

func requireDocker() (string, error) {
	p, err := exec.LookPath("docker")
	if err != nil {
		return "", fmt.Errorf("docker not found on PATH: install Docker Desktop / Docker Engine first")
	}
	return p, nil
}

func cmdDockerUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docker up", flag.ExitOnError)
	image := fs.String("image", envOr("GATT_DOCKER_IMAGE", defaultDockerImage), "Docker image to run")
	name := fs.String("name", defaultDockerContainer, "container name")
	port := fs.Int("port", defaultDockerPort, "host loopback port (bound strictly to 127.0.0.1)")
	var mounts multiFlag
	fs.Var(&mounts, "mount", "host directory to mount read-only at the same absolute path (repeatable; default: $HOME)")
	repos := fs.String("repos", "", "optional comma-separated absolute repo paths to preload on startup")
	pull := fs.Bool("pull", false, "pull the image before starting")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dockerBin, err := requireDocker()
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("could not determine home directory (pass --mount DIR): %w", err)
		}
		mounts = append(mounts, home)
	}

	if *pull {
		p := exec.CommandContext(ctx, dockerBin, "pull", *image)
		p.Stdout, p.Stderr = os.Stdout, os.Stderr
		if err := p.Run(); err != nil {
			return fmt.Errorf("docker pull failed: %w", err)
		}
	}

	// Replace any existing container with the same name (named volumes persist).
	rm := exec.CommandContext(ctx, dockerBin, "rm", "-f", *name)
	_ = rm.Run()

	runArgs := []string{
		"run", "-d",
		"--name", *name,
		"--restart", "unless-stopped",
		"-p", fmt.Sprintf("127.0.0.1:%d:7788", *port),
		"-v", "gatt-data:/data",
		"-v", "gatt-cache:/cache/huggingface",
	}
	for _, m := range mounts {
		abs, err := filepath.Abs(m)
		if err != nil {
			return fmt.Errorf("--mount %q: %w", m, err)
		}
		runArgs = append(runArgs, "-v", fmt.Sprintf("%s:%s:ro", abs, abs))
	}
	if strings.TrimSpace(*repos) != "" {
		runArgs = append(runArgs, "-e", "GATT_REPOS="+strings.TrimSpace(*repos))
	}
	runArgs = append(runArgs, *image)

	cmd := exec.CommandContext(ctx, dockerBin, runArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("docker run failed: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	containerID := strings.TrimSpace(string(out))
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", *port)
	fmt.Fprintf(os.Stderr, "gatt docker: started %s (%s), waiting for %s/health...\n", *name, containerID, baseURL)

	hc := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(baseURL + "/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fmt.Printf("gatt docker: ready at %s (%s)\n", baseURL, strings.TrimSpace(string(body)))
				fmt.Printf("next: in any mounted repo, run: gatt install --http %s\n", baseURL)
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("container %s started but %s/health did not become ready within 30s (check `gatt docker logs`)", *name, baseURL)
}

func cmdDockerDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docker down", flag.ExitOnError)
	name := fs.String("name", defaultDockerContainer, "container name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dockerBin, err := requireDocker()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, dockerBin, "rm", "-f", *name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker rm -f %s failed: %w", *name, err)
	}
	fmt.Printf("gatt docker: stopped %s (volumes gatt-data and gatt-cache preserved)\n", *name)
	return nil
}

func cmdDockerStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docker status", flag.ExitOnError)
	port := fs.Int("port", defaultDockerPort, "host loopback port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/status", *port)
	hc := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("gatt server not reachable at %s (start it with `gatt docker up`): %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		fmt.Println(pretty.String())
		return nil
	}
	fmt.Println(string(body))
	return nil
}

func cmdDockerLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docker logs", flag.ExitOnError)
	name := fs.String("name", defaultDockerContainer, "container name")
	follow := fs.Bool("f", false, "follow log output")
	tail := fs.Int("tail", 50, "number of lines to show from the end of the logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dockerBin, err := requireDocker()
	if err != nil {
		return err
	}
	logArgs := []string{"logs", "--tail", strconv.Itoa(*tail)}
	if *follow {
		logArgs = append(logArgs, "-f")
	}
	logArgs = append(logArgs, *name)
	cmd := exec.CommandContext(ctx, dockerBin, logArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdDockerPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docker pull", flag.ExitOnError)
	image := fs.String("image", envOr("GATT_DOCKER_IMAGE", defaultDockerImage), "Docker image to pull")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dockerBin, err := requireDocker()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, dockerBin, "pull", *image)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
