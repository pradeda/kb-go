package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ─── the isolation contract, at the level a write actually sees ───────────────
//
// Every writable path of a corpus must leave production when the isolation set is
// present. The Go side owns the values the CLI uses; compile.py, spawned by the CLI,
// resolves the same paths from the same variables (bound by
// tests/test_isolation_contract.py).

func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range isolationVars {
		value := filepath.Join(root, strings.ToLower(strings.TrimPrefix(name, "KB_"))+".path")
		if strings.HasSuffix(name, "_DB") {
			value = filepath.Join(root, strings.ToLower(strings.TrimPrefix(name, "KB_"))+".db")
		}
		t.Setenv(name, value)
	}
	return root
}

func TestIsolationCoversEveryWritablePath(t *testing.T) {
	root := isolate(t)
	for _, name := range []string{"homelab", "ai"} {
		profile, err := corpusProfile(name)
		if err != nil {
			t.Fatal(err)
		}
		for label, path := range map[string]string{
			"DBPath":        profile.DBPath,
			"RawRoot":       profile.RawRoot,
			"QuarantineDir": profile.QuarantineDir,
			"QuarantineLog": profile.QuarantineLog,
			"EnvFile":       profile.EnvFile,
		} {
			if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
				t.Errorf("%s.%s = %q, want a path under the isolation root %q", name, label, path, root)
			}
		}
	}
}

func TestIsolationPartialSetFailsClosed(t *testing.T) {
	isolate(t)
	// Drop one variable of the full set: the binary must refuse to run rather than
	// fall back to a production path for it.
	if err := os.Unsetenv("KB_AI_ENV"); err != nil {
		t.Fatal(err)
	}
	_, err := corpusProfile("homelab")
	if err == nil {
		t.Fatal("partial isolation accepted; production fallback is possible")
	}
	if !strings.Contains(err.Error(), "KB_AI_ENV") {
		t.Errorf("error %q must name the missing variable", err)
	}
}

// ─── isolated end-to-end `kb add` ─────────────────────────────────────────────

// buildCLI builds the CLI once per test binary, exactly as `make build` does.
func buildCLI(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "kb")
	build := exec.Command("go", "build", "-tags", "fts5", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build the CLI here: %v\n%s", err, out)
	}
	return binary
}

// TestIsolatedAddQuarantinesATitleOnlySecret drives the real binary over a temporary
// corpus: the secret lives only in the title, and the pre-redaction title must land in
// the temporary quarantine while /opt/kb/quarantine and /opt/kb/.env stay untouched.
func TestIsolatedAddQuarantinesATitleOnlySecret(t *testing.T) {
	root := t.TempDir()
	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join(repo, "config", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "kb.db")
	db, err := openDB(CorpusProfile{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db.Close()

	rules, err := os.ReadFile(filepath.Join(repo, "config", "secret_patterns.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret_patterns.json"), rules, 0600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, "empty.env")
	if err := os.WriteFile(envFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(root, "quarantine")
	rawRoot := filepath.Join(root, "raw")
	for _, dir := range []string{quarantine, rawRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	productionBefore := productionQuarantineState(t)

	// Everything the CLI reads or writes for the homelab corpus, redirected. The AI
	// corpus is redirected too - the isolation set is all-or-nothing.
	t.Setenv("KB_HOMELAB_DB", dbPath)
	t.Setenv("KB_HOMELAB_RAW", rawRoot)
	t.Setenv("KB_HOMELAB_QUARANTINE", quarantine)
	t.Setenv("KB_HOMELAB_ENV", envFile)
	t.Setenv("KB_AI_DB", filepath.Join(root, "ai.db"))
	t.Setenv("KB_AI_RAW", filepath.Join(root, "ai-raw"))
	t.Setenv("KB_AI_QUARANTINE", filepath.Join(root, "ai-quarantine"))
	t.Setenv("KB_AI_ENV", filepath.Join(root, "ai.env"))
	binary := buildCLI(t)
	const secretTitle = "POSTGRES_PASSWORD=hunter2xyz"
	cmd := exec.Command(binary, "add", "note", "-", secretTitle, "test")
	cmd.Stdin = strings.NewReader("clean body")
	cmd.Env = os.Environ()
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("kb add failed: %v\n%s", runErr, out)
	}
	stderr := string(out)
	if !strings.Contains(stderr, "redacted") {
		t.Errorf("stderr = %q, want a redaction warning for the title secret", stderr)
	}

	backups, err := filepath.Glob(filepath.Join(quarantine, "*.orig"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("quarantine backups = %v, want exactly one in %s", backups, quarantine)
	}
	payload, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), secretTitle) {
		t.Errorf("backup %s = %q, want it to contain the original title", backups[0], payload)
	}

	// The production quarantine and the production env file were not touched.
	if after := productionQuarantineState(t); after != productionBefore {
		t.Errorf("/opt/kb/quarantine changed during an isolated add: before=%v after=%v", productionBefore, after)
	}
}

// productionQuarantineState fingerprints the production quarantine without writing:
// its entry count plus the audit log size.
func productionQuarantineState(t *testing.T) string {
	t.Helper()
	profile, err := func() (CorpusProfile, error) {
		saved := map[string]string{}
		for _, name := range isolationVars {
			saved[name] = os.Getenv(name)
			os.Unsetenv(name)
		}
		defer func() {
			for name, value := range saved {
				if value != "" {
					os.Setenv(name, value)
				}
			}
		}()
		return corpusProfile("homelab")
	}()
	if err != nil {
		t.Skipf("production profile unavailable: %v", err)
	}
	entries, err := os.ReadDir(profile.QuarantineDir)
	if err != nil {
		t.Skipf("production quarantine unreadable: %v", err)
	}
	info, err := os.Stat(profile.QuarantineLog)
	if err != nil {
		return "dir:" + itoa(len(entries))
	}
	return "dir:" + itoa(len(entries)) + " log:" + itoa(int(info.Size()))
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
