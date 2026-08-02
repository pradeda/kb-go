package main

import "fmt"

const defaultCorpus = "homelab"

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

func corpusProfile(name string) (CorpusProfile, error) {
	switch name {
	case "homelab":
		return CorpusProfile{
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
		}, nil
	case "ai":
		return CorpusProfile{
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
		}, nil
	default:
		return CorpusProfile{}, fmt.Errorf("unknown corpus %q (allowed: homelab, ai)", name)
	}
}

func (p CorpusProfile) sqliteDSN() string {
	return p.DBPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
}

func (p CorpusProfile) compileCommand() string {
	if p.Name == defaultCorpus {
		return "python3 /opt/kb/compile.py"
	}
	return "python3 /opt/kb/compile.py --corpus " + p.Name
}
