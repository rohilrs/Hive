package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/pkg/rpc"
	"github.com/spf13/cobra"
)

var (
	repoConfigPathOnly bool
	repoConfigInit     bool
)

// repoPathForSlug resolves a project's repo_path via project.list (there is no
// project.get RPC). project.list returns a bare []rpc.ProjectView.
func repoPathForSlug(slug string) (string, error) {
	raw, err := rpcCallRaw(rpc.MethodListProjects, map[string]any{})
	if err != nil {
		return "", err
	}
	var projs []rpc.ProjectView
	if err := json.Unmarshal(raw, &projs); err != nil {
		return "", fmt.Errorf("decode projects: %w", err)
	}
	for _, p := range projs {
		if p.Slug == slug {
			if p.RepoPath == "" {
				return "", fmt.Errorf("project %q has no repo path", slug)
			}
			return p.RepoPath, nil
		}
	}
	return "", fmt.Errorf("project not found: %s", slug)
}

func newRepoCmd() *cobra.Command {
	repoCmd := &cobra.Command{
		Use:   "repo",
		Short: "Per-repo config (shared by all projects on a repo)",
	}
	cfgCmd := &cobra.Command{
		Use:   "config <slug>",
		Short: "Show, locate, or seed the per-repo config layer for a project's repo",
		Long: "Resolves the repo of project <slug> and prints its per-repo config layer\n" +
			"(~/.hive/repos/<key>/config.toml), merged between global and per-project\n" +
			"config for every project on that repo.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			repoPath, err := repoPathForSlug(args[0])
			if err != nil {
				return err
			}
			key := config.RepoKey(repoPath)
			hiveDir, err := resolveHiveDir()
			if err != nil {
				return err
			}
			path := filepath.Join(hiveDir, "repos", key, "config.toml")

			if repoConfigPathOnly {
				fmt.Println(path)
				return nil
			}
			if repoConfigInit {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
					stub := fmt.Sprintf("# repo: %s\n"+
						"# Hive per-repo config layer — merged between global and per-project\n"+
						"# config for every project on this repo. Put repo-wide settings here\n"+
						"# (e.g. [pipelines.build]/[pipelines.finish_branch] commands).\n", repoPath)
					if werr := os.WriteFile(path, []byte(stub), 0o644); werr != nil {
						return werr
					}
					fmt.Printf("created %s\n", path)
				} else {
					fmt.Printf("exists: %s\n", path)
				}
				return nil
			}

			body, rerr := os.ReadFile(path)
			if os.IsNotExist(rerr) {
				fmt.Printf("%s\n(no per-repo config yet — create it there, or run `hive repo config %s --init`)\n", path, args[0])
				return nil
			}
			if rerr != nil {
				return rerr
			}
			fmt.Printf("%s\n\n%s", path, string(body))
			return nil
		},
	}
	cfgCmd.Flags().BoolVar(&repoConfigPathOnly, "path", false, "print only the file path")
	cfgCmd.Flags().BoolVar(&repoConfigInit, "init", false, "create the repo config dir + a stub file if absent")
	cfgCmd.MarkFlagsMutuallyExclusive("path", "init")
	repoCmd.AddCommand(cfgCmd)
	return repoCmd
}
