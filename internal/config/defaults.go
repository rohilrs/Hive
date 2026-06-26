// Package config owns Hive's TOML configuration with a four-layer
// precedence: compiled defaults → ~/.hive/config.toml →
// ~/.hive/projects/<slug>/config.toml → HIVE_* env vars → CLI flags.
package config

// Default returns a Config populated with compiled-in defaults.
// All values here should match spec §12.2.
func Default() *Config {
	return &Config{
		Concurrency: Concurrency{
			MaxWorkers:        3,
			PerRepoCap:        2,
			PerRepoCapNoGuard: 1,
			FinishBranchCap:   1,
			PlanCap:           2,
		},
		Costs: Costs{
			CapPerRunUSD: 10.0,
			CapPerDayUSD: 50.0,
			AlertAtPct:   80,
		},
		Models: Models{
			WorkerLadder:           []string{"claude-sonnet-4-6", "claude-sonnet-4-6", "claude-opus-4-7"},
			ReviewerLadder:         []string{"claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-7"},
			Classifier:             "claude-haiku-4-5",
			ChatDefault:            "claude-haiku-4-5",
			ChatReasoning:          "claude-sonnet-4-6",
			DecomposeModel:         "claude-sonnet-4-6",
			GraduateValidatorModel: "",
		},
		Pipelines: Pipelines{
			Build: BuildPipeline{
				MaxIterations:        3,
				PrefetchBudgetTokens: 3000,
				StageTimeoutMinutes:  20,
				ConflictHopDepth:     1,
				Documenter: BuildDocumenter{
					Enabled:             true,
					Model:               "claude-sonnet-4-6",
					StageTimeoutMinutes: 10,
					CodeComments:        false,
					UpdateReadme:        false,
					SkillsToLoad:        []string{}, // self-contained prompt; opt in per project (a missing skill hard-fails scope + silently skips docs)
				},
				TestCommand:                 "go test ./...",
				ValidateCommand:             "go build ./... && go vet ./...",
				TestStageTimeoutMinutes:     10,
				ValidateStageTimeoutMinutes: 5,
			},
			Debug: SimplePipeline{MaxIterations: 3, ConflictHopDepth: 0},
			Plan:  SimplePipeline{MaxIterations: 3, ConflictHopDepth: 2},
			FinishBranch: FinishBranchPipeline{
				TypecheckCommand:        "go build ./...",
				LintCommand:             "go vet ./...",
				FormatCommand:           "",
				TestCommand:             "go test ./...",
				PrepareCommand:          "",
				CreatePRCommand:         "git push -u origin HEAD && gh pr create --fill --base {{target_branch}}",
				CIMonitorCommand:        "gh pr checks --watch",
				StageTimeoutMinutes:     10,
				CIMonitorTimeoutMinutes: 30,
				ConflictHopDepth:        0,
				MaxFixAttempts:          2,
			},
			Resolve: ResolvePipeline{
				Auto:                false,
				MaxIterations:       5,
				StageTimeoutMinutes: 20,
			},
			Graduate: GraduatePipeline{
				AuditRuns: defaultGraduateAuditRuns,
			},
		},
		Predictor: Predictor{
			Enabled:                true,
			BundleTokenCap:         6000,
			MaxCandidates:          10,
			PerCallTimeoutSeconds:  5,
			HaikuTimeoutSeconds:    10,
			HaikuModel:             "claude-haiku-4-5",
			PrecisionKillThreshold: 0.5,
			RollingWindowDays:      30,
			ForceEnable:            false,
		},
		ConflictGuard: ConflictGuard{
			Enabled: true,
		},
		LLM: LLM{
			Provider: "cli",
		},
		TUI: TUI{
			EventBufferSize:  1000,
			ResyncOnOverflow: true,
			HeartbeatSeconds: 5,
		},
		StallDetection: StallDetection{
			EventHeartbeatSeconds:      60,
			ToolCallMaxMinutes:         5,
			ImplementStagnationMinutes: 8,
			LoopCheckAfterIter:         1,
			LoopSimilarityThreshold:    0.85,
			NotifyOnStall:              false,
		},
		Approvals: Approvals{
			Enabled:            false,
			Mode:               "ask",
			HookTimeoutSeconds: 300,
			DefaultAllowWorker: []string{"Read", "Edit", "Write", "MultiEdit", "Grep", "Glob"},
			// Safe-by-default Bash allow-list (globs; * spans any chars).
			// Read/inspect + project toolchains + read-only git. Destructive
			// or network commands (rm, mv, curl, wget, chmod, kill, sudo,
			// git push/commit) are intentionally absent — opt in per project
			// via config or `hive approvals allow Bash --glob '...'`.
			DefaultAllowWorkerBash: []string{
				"ls*", "cat *", "head *", "tail *", "wc *", "find *", "grep *",
				"which *", "pwd*", "echo *", "sort *", "uniq *", "cut *", "tr *",
				"dirname *", "basename *", "realpath *", "stat *", "file *", "tree *",
				"diff *", "test *", "date*", "env*", "sed *", "awk *", "jq *",
				"go *", "gofmt *", "node *", "npm *", "pnpm *", "npx *",
				"python *", "python3 *", "pip *", "pytest *", "tsc *", "cargo *", "make *",
				"git status*", "git diff*", "git log*", "git show*", "git branch*",
				"git rev-parse*", "git ls-files*", "git stash list*",
			},
			DefaultAllowReviewer: []string{"Read", "Grep", "Glob"},
		},
		Chat: Chat{
			Provider:              "api",
			DefaultModel:          "claude-haiku-4-5",
			ReasoningModel:        "claude-sonnet-4-6",
			APIKeyEnv:             "ANTHROPIC_API_KEY",
			MaxIters:              8,
			ConfirmTimeoutSeconds: 300,
			AutoConfirm: []string{
				"hive_list_tasks", "hive_get_task", "hive_get_run",
				"hive_active_workers", "hive_cost_summary", "hive_status",
				"hive_search", "hive_show_diff", "hive_predict", "hive_attach_run",
			},
			OpenSessionStaleHours: 1,
		},
		Scavenger: Scavenger{
			Enabled:              true,
			Binary:               "scavenger",
			PluginDir:            ".scavenger/claude-plugin",
			MCPSocketEnv:         "SCAVENGER_SOCK",
			IndexWorktreeOnRun:   true,
			MaxConcurrentDaemons: 8,
			IndexTimeoutSeconds:  120,
		},
		ClaudeCLI: ClaudeCLI{
			Binary: "claude",
		},
		Anthropic: Anthropic{
			RetryMaxAttempts:                    5,
			RetryBackoffInitialMS:               500,
			RetryBackoffMaxMS:                   30000,
			PausePipelineAfterConsecutiveErrors: 3,
		},
		Sources: Sources{
			Inbox:  SourceConfig{SyncIntervalMinutes: 5},
			GitHub: SourceConfig{SyncIntervalMinutes: 30},
			Linear: LinearConfig{SyncIntervalMinutes: 15, APIKeyEnv: "LINEAR_API_KEY"},
		},
	}
}
