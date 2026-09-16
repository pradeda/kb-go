package main

import (
	"fmt"
	"os"
	"strings"
)

const defaultCorpus = "homelab"
const compilePython = "/opt/kb/venv-embed/bin/python"

// CorpusProfile is the complete allowlisted storage contract for one corpus.
// Callers select a profile by name; no CLI option accepts an arbitrary path.
type CorpusProfile struct {
	Name               string
	DBPath             string
	RawRoot            string
	EnvFile            string
	ChromaCollection   string
	WikiIndexPath      string
	SecretPatternsPath string
	QuarantineDir      string
	QuarantineLog      string
	WatcherLock        string
	WatcherState       string
}

// isolationVars is the complete storage-override set. Isolation is all-or-
// nothing: if ANY of these is set, EVERY one must be, or corpusProfile fails —
// an isolated run (tests, the Go entry-point test) can never fall through to a
// production path. compile.py's _apply_isolation_env mirrors this list exactly,
// so the Go binary and every compile.py subprocess it spawns resolve identical
// paths. The corpora keep SEPARATE raw roots in production, so raw is overridden
// per corpus rather than through one shared KB_RAW.
var isolationVars = []string{"KB_HOMELAB_DB", "KB_AI_DB", "KB_HOMELAB_RAW", "KB_AI_RAW"}

// isolationOverride returns nil for production paths (no vars set) or the full
// override map, and errors on a partial set rather than silently using prod.
func isolationOverride() (map[string]string, error) {
	present := map[string]string{}
	any := false
	for _, k := range isolationVars {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			present[k] = v
			any = true
		}
	}
	if !any {
		return nil, nil
	}
	var missing []string
	for _, k := range isolationVars {
		if present[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("incomplete KB isolation env: set all of %v (missing %v); refusing production fallback", isolationVars, missing)
	}
	return present, nil
}

func corpusProfile(name string) (CorpusProfile, error) {
	var p CorpusProfile
	switch name {
	case "homelab":
		p = CorpusProfile{
			Name:               "homelab",
			DBPath:             "/opt/kb/kb.db",
			RawRoot:            "/opt/kb/raw",
			EnvFile:            "/opt/kb/.env",
			ChromaCollection:   "kb_collection",
			WikiIndexPath:      "/opt/kb/wiki/index.md",
			SecretPatternsPath: "/opt/kb/secret_patterns.json",
			QuarantineDir:      "/opt/kb/quarantine",
			QuarantineLog:      "/opt/kb/quarantine.log",
			WatcherLock:        "/tmp/kb-watcher.lock",
			WatcherState:       "/tmp/kb-watcher-last",
		}
	case "ai":
		p = CorpusProfile{
			Name:               "ai",
			DBPath:             "/opt/ai-kb/ai-kb.db",
			RawRoot:            "/opt/ai-kb/raw",
			EnvFile:            "/opt/ai-kb/.env",
			ChromaCollection:   "ai_kb_collection",
			WikiIndexPath:      "",
			SecretPatternsPath: "/opt/ai-kb/secret_patterns.json",
			QuarantineDir:      "/opt/ai-kb/quarantine",
			QuarantineLog:      "/opt/ai-kb/quarantine.log",
			WatcherLock:        "/tmp/ai-kb-watcher.lock",
			WatcherState:       "/tmp/ai-kb-watcher-last",
		}
	default:
		return CorpusProfile{}, fmt.Errorf("unknown corpus %q (allowed: homelab, ai)", name)
	}

	ov, err := isolationOverride()
	if err != nil {
		return CorpusProfile{}, err
	}
	if ov != nil {
		switch name {
		case "homelab":
			p.DBPath = ov["KB_HOMELAB_DB"]
			p.RawRoot = ov["KB_HOMELAB_RAW"]
		case "ai":
			p.DBPath = ov["KB_AI_DB"]
			p.RawRoot = ov["KB_AI_RAW"]
		}
	}
	return p, nil
}

func (p CorpusProfile) sqliteDSN() string {
	return p.DBPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
}

// compileArgv builds the argv for a compile.py call against this corpus.
// Everything that needs to run compile.py goes through here so the --corpus
// routing cannot drift between call sites: compile.py applies configure_corpus
// before it dispatches, so a dropped flag silently operates on homelab.
func (p CorpusProfile) compileArgv(extra ...string) []string {
	// KB_COMPILE_PYTHON / KB_COMPILE_PY select WHICH code runs, orthogonal to
	// the data-isolation set above: the entry-point test points the locally
	// built binary at runtime/compile.py before it is deployed. Both default to
	// the production install.
	python := compilePython
	if v := strings.TrimSpace(os.Getenv("KB_COMPILE_PYTHON")); v != "" {
		python = v
	}
	script := "/opt/kb/compile.py"
	if v := strings.TrimSpace(os.Getenv("KB_COMPILE_PY")); v != "" {
		script = v
	}
	argv := []string{python, script}
	if p.Name != defaultCorpus {
		argv = append(argv, "--corpus", p.Name)
	}
	return append(argv, extra...)
}

// compileCommand renders the same call as a copy-pasteable string for humans.
func (p CorpusProfile) compileCommand() string {
	return strings.Join(p.compileArgv(), " ")
}
