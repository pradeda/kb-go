package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	defaultModel  = "google/gemini-2.5-flash-lite"
	// KB_SEARCH_API_URL overrides this, from the shell or from the profile
	// env file. Set it when the Search API runs on a different host.
	defaultKBSearchBase = "http://localhost:8050"
	// Scope for `kb ask` without --scope. Not "auto", which the router still
	// rejects, and no longer "homelab", which hid the AI corpus from every
	// caller that did not know the flag existed.
	defaultAskScope = "both"
)

var (
	httpQuick     = &http.Client{Timeout: 30 * time.Second}
	httpSlow      = &http.Client{Timeout: 150 * time.Second}
	kbSearchAPIV2 = defaultKBSearchBase + "/v2/kb/search"
)

// applyKBSearchBase has to run after loadEnv, not during package init: the env
// file is read inside main, so resolving these at init would see only a
// shell-exported value and silently ignore the file.
func applyKBSearchBase() {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("KB_SEARCH_API_URL")), "/")
	if base == "" {
		return
	}
	kbSearchAPIV2 = base + "/v2/kb/search"
}

type Result struct {
	ID      int64
	Title   string
	Content string
	Summary string
	Tags    string
	Source  string
	Date    string
	Score   float64
}

type commandInvocation struct {
	Command string
	Args    []string
	Profile CorpusProfile
	Scope   string
	Alt     string
	AltLang string
}

type singleCorpusFlag struct {
	value *string
	set   bool
}

type singleScopeFlag struct {
	value *string
	set   bool
}

type singleAskValueFlag struct {
	value *string
	name  string
	set   bool
}

func (f *singleAskValueFlag) String() string {
	if f == nil || f.value == nil {
		return ""
	}
	return *f.value
}

func (f *singleAskValueFlag) Set(value string) error {
	if f.set {
		return fmt.Errorf("--%s may be specified only once", f.name)
	}
	f.set = true
	*f.value = value
	return nil
}

func (f *singleScopeFlag) String() string {
	if f == nil || f.value == nil {
		return ""
	}
	return *f.value
}

func (f *singleScopeFlag) Set(value string) error {
	if f.set {
		return errors.New("--scope may be specified only once")
	}
	f.set = true
	*f.value = value
	return nil
}

func (f *singleCorpusFlag) String() string {
	if f == nil || f.value == nil {
		return defaultCorpus
	}
	return *f.value
}

func (f *singleCorpusFlag) Set(value string) error {
	if f.set {
		return errors.New("--corpus may be specified only once")
	}
	f.set = true
	*f.value = value
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	invocation, err := parseInvocation(os.Args[1:])
	if err != nil {
		if err.Error() != "usage" {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		printUsage()
		os.Exit(1)
	}
	loadEnv(invocation.Profile.EnvFile)
	applyKBSearchBase()

	switch invocation.Command {
	case "ask":
		cmdAsk(ctx, invocation.Args, invocation.Profile, invocation.Scope, invocation.Alt, invocation.AltLang)
	case "add":
		cmdAdd(ctx, invocation.Args, invocation.Profile)
	case "list":
		cmdList(ctx, invocation.Args, invocation.Profile)
	case "search":
		cmdSearch(ctx, invocation.Args, invocation.Profile)
	case "pending":
		cmdPending(ctx, invocation.Profile)
	case "retire":
		cmdRetire(ctx, invocation.Args, invocation.Profile)
	}
}

func parseInvocation(args []string) (commandInvocation, error) {
	if len(args) == 0 {
		return commandInvocation{}, errors.New("usage")
	}

	command := args[0]
	switch command {
	case "ask", "add", "list", "search", "pending", "retire":
	default:
		return commandInvocation{}, fmt.Errorf("unknown command %q", command)
	}

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	corpusName := defaultCorpus
	scope := ""
	alt := ""
	altLang := ""
	if command == "ask" {
		fs.Var(&singleScopeFlag{value: &scope}, "scope", "search scope (homelab, ai, both, or auto)")
		fs.Var(&singleAskValueFlag{value: &alt, name: "alt"}, "alt", "same question in the other language")
		fs.Var(&singleAskValueFlag{value: &altLang, name: "alt-lang"}, "alt-lang", "alternate language (sr or en)")
	} else {
		fs.Var(&singleCorpusFlag{value: &corpusName}, "corpus", "target corpus (homelab or ai)")
	}
	commandArgs := args[1:]
	flagArgs := []string{}
	if command == "ask" {
		for index := 0; index < len(commandArgs); {
			arg := commandArgs[index]
			isEquals := strings.HasPrefix(arg, "--scope=") || strings.HasPrefix(arg, "--alt=") || strings.HasPrefix(arg, "--alt-lang=")
			isSeparate := arg == "--scope" || arg == "--alt" || arg == "--alt-lang"
			if !isEquals && !isSeparate {
				break
			}
			flagArgs = append(flagArgs, arg)
			index++
			if isSeparate && index < len(commandArgs) {
				flagArgs = append(flagArgs, commandArgs[index])
				index++
			}
		}
	} else if command != "ask" && len(commandArgs) > 0 &&
		(commandArgs[0] == "--corpus" || strings.HasPrefix(commandArgs[0], "--corpus=")) {
		flagArgs = commandArgs
	}
	if err := fs.Parse(flagArgs); err != nil {
		return commandInvocation{}, err
	}
	positionalArgs := commandArgs
	if command == "ask" {
		positionalArgs = commandArgs[len(flagArgs):]
	} else if len(flagArgs) > 0 {
		positionalArgs = fs.Args()
	}
	for _, arg := range positionalArgs {
		if arg == "--corpus" || strings.HasPrefix(arg, "--corpus=") {
			return commandInvocation{}, errors.New("--corpus must appear immediately after the command")
		}
		if arg == "--scope" || strings.HasPrefix(arg, "--scope=") {
			return commandInvocation{}, errors.New("--scope is valid only for ask and must appear immediately after the command")
		}
		if arg == "--alt" || strings.HasPrefix(arg, "--alt=") || arg == "--alt-lang" || strings.HasPrefix(arg, "--alt-lang=") {
			return commandInvocation{}, errors.New("--alt and --alt-lang must appear before the question")
		}
		// Preserve the frozen legacy `kb ask --help` query while rejecting new,
		// silently swallowed long options such as a misplaced --alt.
		if command == "ask" && arg != "--help" && bareLongOption.MatchString(arg) {
			return commandInvocation{}, fmt.Errorf("unknown or misplaced option %q", arg)
		}
	}
	if scope != "" && scope != "homelab" && scope != "ai" && scope != "both" && scope != "auto" {
		return commandInvocation{}, fmt.Errorf("unknown scope %q (allowed: homelab, ai, both, auto)", scope)
	}
	if (alt == "") != (altLang == "") {
		return commandInvocation{}, errors.New("--alt and --alt-lang must be supplied together")
	}
	if altLang != "" && altLang != "sr" && altLang != "en" {
		return commandInvocation{}, fmt.Errorf("unknown alternate language %q (allowed: sr or en)", altLang)
	}

	profile, err := corpusProfile(corpusName)
	if err != nil {
		return commandInvocation{}, err
	}
	return commandInvocation{Command: command, Args: positionalArgs, Profile: profile, Scope: scope, Alt: alt, AltLang: altLang}, nil
}

func printUsage() {
	fmt.Println(`Usage: kb <command> [args...]

Commands:
  ask [--scope homelab|ai|both|auto] [--alt "other-language question" --alt-lang sr|en] "question"
  add [--corpus homelab|ai] note "content" "title" "tags"
  add [--corpus homelab|ai] note - "title" "tags"   Add from stdin
  add [--corpus homelab|ai] url "url" "title" "tags"
  list [--corpus homelab|ai] [limit]                  List entries
  search [--corpus homelab|ai] "query" [limit]        FTS5 search
  pending [--corpus homelab|ai]                       Pending entries
  retire [--corpus homelab|ai] <id>                   Delete one entry (all layers)

For --alt, faithfully translate the same intent into the other language. Do not
add facts or broaden/narrow it; preserve technical literals exactly.

Examples:
  kb ask "how does NFS work in the homelab?"
  kb add note "Docker tip" "Use restart: always" "docker,tips"
  kb add note - "My Note" "tag1" <<'EOF'
  ...long content...
  EOF
  kb add url "https://example.com" "Interesting article" "bookmarks"
  kb list 10
  kb search "docker"
  kb search --corpus ai "transformers" 10
  kb pending
  kb retire 584`)
}

// ─── ask ──────────────────────────────────────────────────────────────────────

func cmdAsk(ctx context.Context, args []string, profile CorpusProfile, invocationScope, alt, altLang string) {
	start := time.Now()

	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kb ask \"question\"")
		os.Exit(1)
	}

	query := strings.Join(args, " ")
	if err := runAsk(ctx, query, invocationScope, alt, altLang, profile, start); err != nil {
		os.Exit(1)
	}
}

func runAsk(ctx context.Context, query, invocationScope, alt, altLang string, profile CorpusProfile, start time.Time) error {
	scope := invocationScope
	if scope == "" {
		scope = defaultAskScope
	}
	return cmdAskV2(ctx, query, alt, altLang, scope, profile, start)
}

// ─── add ──────────────────────────────────────────────────────────────────────

// bareLongOption matches a bare long option such as --title or --tags.
// It deliberately does not match "---" or "--- a/file.go": YAML frontmatter and
// diff headers are legitimate note content and must stay storable.
var bareLongOption = regexp.MustCompile(`^--[A-Za-z][A-Za-z0-9-]*$`)

// addSlotNames labels the positional data slots of `kb add`, in order, so a
// rejection can say which one was mistyped.
var addSlotNames = [...]string{"content", "title", "tags"}

// checkAddArgShape rejects an invocation that was typed as if `kb add` took
// options. It does not, and parseInvocation has already consumed the only flag
// there is (--corpus), so a bare long option in a data slot is always a
// mistyped command and never data.
//
// This exists because entries 606-608 (2026-08-11) were written by a
// `kb add note "<body>" --title "<real title>"` that shifted every argument one
// slot: the title column got the literal string "--title", tags got the
// intended title, and the real tags were never passed at all. Nothing
// downstream inspects title quality — compile, embed and even `--health` call
// such a row perfectly healthy, since it has both an SQLite row and a vector —
// so the corruption sat in the corpus for a day and was found by accident.
func checkAddArgShape(args []string) error {
	if len(args) > len(addSlotNames)+1 {
		return fmt.Errorf("too many arguments (%d); at most <type> <content> <title> <tags>", len(args))
	}
	for i, arg := range args[1:] {
		if bareLongOption.MatchString(arg) {
			return fmt.Errorf("%q is an option, not a value, in the %s slot", arg, addSlotNames[i])
		}
	}
	return nil
}

func cmdAdd(ctx context.Context, args []string, profile CorpusProfile) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kb add <note|url> ...")
		fmt.Fprintln(os.Stderr, "  kb add note \"content\" \"title\" \"tags\"")
		fmt.Fprintln(os.Stderr, "  kb add note - \"title\" \"tags\"  (read content from stdin)")
		fmt.Fprintln(os.Stderr, "  kb add url \"url\" \"title\" \"tags\"")
		os.Exit(1)
	}

	if err := ctx.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Aborted: %v\n", err)
		os.Exit(1)
	}

	entryType := args[0]
	if entryType != "note" && entryType != "url" {
		fmt.Fprintf(os.Stderr, "Invalid type: %s (must be 'note' or 'url')\n", entryType)
		os.Exit(1)
	}

	if err := checkAddArgShape(args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintln(os.Stderr, "kb add takes positional arguments only:")
		fmt.Fprintln(os.Stderr, "  kb add note \"content\" \"title\" \"tags\"")
		fmt.Fprintln(os.Stderr, "  kb add note - \"title\" \"tags\"  (read content from stdin)")
		fmt.Fprintln(os.Stderr, "  kb add url \"url\" \"title\" \"tags\"")
		os.Exit(1)
	}

	var content, title, tags string

	if entryType == "note" && len(args) > 1 && args[1] == "-" {
		if len(args) >= 3 {
			title = args[2]
		}
		if len(args) >= 4 {
			tags = args[3]
		}
		stdinBytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
			os.Exit(1)
		}
		content = string(stdinBytes)
		if content == "" {
			fmt.Fprintln(os.Stderr, "Error: no content provided on stdin")
			os.Exit(1)
		}
	} else {
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Error: content is required for %s\n", entryType)
			os.Exit(1)
		}
		content = args[1]
		if len(args) >= 3 {
			title = args[2]
		}
		if len(args) >= 4 {
			tags = args[3]
		}
	}

	// Gate 1: redact secrets before content reaches SQLite/FTS or the raw file.
	origContent := content
	var scanHits []scanHit
	var rerr error
	content, title, scanHits, rerr = sanitizeWrite(profile, content, title)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "Error: secret scanner unavailable for corpus %s: %v\n", profile.Name, rerr)
		os.Exit(1)
	}

	if title == "" {
		title = firstLine(content, 80)
	}

	now := time.Now()
	ts := now.Format("2006-01-02T15:04:05")
	dateStr := now.Format("2006-01-02")
	slug := slugify(title)
	if slug == "" {
		slug = slugify(content)
	}
	if slug == "" {
		slug = "untitled"
	}

	if len(scanHits) > 0 {
		backup := recordQuarantine(profile, origContent, content, "cli", slug, scanHits)
		names := make([]string, 0, len(scanHits))
		for _, h := range scanHits {
			names = append(names, h.Pattern+"("+h.Action+")")
		}
		fmt.Fprintf(os.Stderr, "kb add: redacted %d secret(s) before write: %s\n",
			len(scanHits), strings.Join(names, ", "))
		if backup != "" {
			fmt.Fprintf(os.Stderr, "         original: %s\n", backup)
		}
	}

	subdir := "notes"
	if entryType == "url" {
		subdir = "urls"
	}

	rawSubdir := fmt.Sprintf("%s/%s", profile.RawRoot, subdir)
	if err := os.MkdirAll(rawSubdir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(rawSubdir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error securing directory: %v\n", err)
		os.Exit(1)
	}
	if err := alignOwnerWithDir(profile.RawRoot, rawSubdir); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not align owner of %s with %s: %v\n", rawSubdir, profile.RawRoot, err)
	}

	frontmatter := fmt.Sprintf(`---
type: %s
title: %s
tags: %s
saved: %s
---

%s
`, entryType, yamlQuote(title), yamlQuote(tags), ts, content)
	rawPath, err := writeRawFileExclusive(rawSubdir, fmt.Sprintf("%s-%s", dateStr, slug), []byte(frontmatter))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
		os.Exit(1)
	}

	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		if rmErr := os.Remove(rawPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warn: could not remove orphan file %s: %v\n", rawPath, rmErr)
		}
		os.Exit(1)
	}
	defer db.Close()

	result, err := db.ExecContext(ctx,
		`INSERT INTO entries (type, content, title, tags, raw_path, source, created_at)
		 VALUES (?, ?, ?, ?, ?, 'cli', ?)`,
		entryType, content, title, tags, rawPath, ts,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inserting into database: %v\n", err)
		if rmErr := os.Remove(rawPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warn: could not remove orphan file %s: %v\n", rawPath, rmErr)
		}
		os.Exit(1)
	}

	id, lastIDErr := result.LastInsertId()
	if lastIDErr != nil {
		fmt.Fprintf(os.Stderr, "warn: could not fetch last insert id: %v\n", lastIDErr)
	}
	fmt.Printf("OK [id=%d] %s\n", id, rawPath)
}

// ─── list ─────────────────────────────────────────────────────────────────────

func cmdList(ctx context.Context, args []string, profile CorpusProfile) {
	limit := 20
	if len(args) >= 1 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT id, type, title, created_at FROM entries
		 ORDER BY created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying database: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	fmt.Printf("%-6s %-6s %-50s %s\n", "ID", "TYPE", "TITLE", "CREATED")
	fmt.Println(strings.Repeat("-", 90))
	for rows.Next() {
		var id int64
		var typ, title, created string
		if err := rows.Scan(&id, &typ, &title, &created); err != nil {
			fmt.Fprintf(os.Stderr, "warn: scan error, skipping row: %v\n", err)
			continue
		}
		fmt.Printf("%-6d %-6s %-50s %s\n", id, typ, truncate(title, 50), created)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}
}

// ─── search ───────────────────────────────────────────────────────────────────

func cmdSearch(ctx context.Context, args []string, profile CorpusProfile) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kb search \"query\" [limit]")
		os.Exit(1)
	}

	query := parseFTSQuery(args[0])
	if query == "" {
		fmt.Fprintln(os.Stderr, "Error: empty search query")
		os.Exit(1)
	}

	limit := 20
	if len(args) >= 2 {
		if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT e.id, e.type, e.title, e.content, e.tags, e.created_at
		 FROM entries e
		 JOIN entries_fts fts ON e.id = fts.rowid
		 WHERE entries_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, query, limit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error searching: %v\n", err)
		fmt.Fprintf(os.Stderr, "Hint: query was escaped to: %s\n", query)
		os.Exit(1)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int64
		var typ, title, content, tags, created string
		if err := rows.Scan(&id, &typ, &title, &content, &tags, &created); err != nil {
			fmt.Fprintf(os.Stderr, "warn: scan error, skipping row: %v\n", err)
			continue
		}
		fmt.Printf("=== [%d] %s | %s | %s ===\n", id, typ, title, created)
		if tags != "" {
			fmt.Printf("Tags: %s\n", tags)
		}
		fmt.Println(truncate(content, 500))
		fmt.Println()
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}

	if count == 0 {
		fmt.Println("No matches found.")
	}
}

// ─── pending ──────────────────────────────────────────────────────────────────

func cmdPending(ctx context.Context, profile CorpusProfile) {
	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT id, type, title, raw_path FROM entries
		 WHERE embedded_at IS NULL ORDER BY created_at`,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying database: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	fmt.Printf("%-6s %-6s %-40s %s\n", "ID", "TYPE", "TITLE", "RAW_PATH")
	fmt.Println(strings.Repeat("-", 100))
	count := 0
	for rows.Next() {
		var id int64
		var typ, title, rawPath string
		if err := rows.Scan(&id, &typ, &title, &rawPath); err != nil {
			fmt.Fprintf(os.Stderr, "warn: scan error, skipping row: %v\n", err)
			continue
		}
		fmt.Printf("%-6d %-6s %-40s %s\n", id, typ, truncate(title, 40), rawPath)
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}

	if count == 0 {
		fmt.Println("All entries embedded.")
	} else {
		fmt.Printf("\n%d entries pending embedding.\nRun: %s\n", count, profile.compileCommand())
	}
}

// ─── retire ───────────────────────────────────────────────────────────────────

// lookupEntryTitle returns the title of one entry, and false if no such row
// exists. A missing row is not an error here: an orphan Chroma vector with no
// SQLite row (entry 584, 2026-08-12) is precisely the state retire has to be
// able to clean up.
func lookupEntryTitle(ctx context.Context, profile CorpusProfile, id int64) (string, bool, error) {
	db, err := openDB(profile)
	if err != nil {
		return "", false, err
	}
	defer db.Close()

	var title string
	err = db.QueryRowContext(ctx, `SELECT title FROM entries WHERE id = ?`, id).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return title, true, nil
}

// cmdRetire shells out to compile.py instead of deleting anything itself.
// kb-go has no Chroma client by design — it speaks only to kb-search-api and
// OpenRouter — and a retire that skipped the vector would leave exactly the
// orphan that --health alarms on. compile.py owns the four-layer ordering
// (vector, SQLite row, FTS5 via trigger, raw file); this subcommand exists only
// so that removing a bad entry has a shorter path than a hand-typed
// DELETE FROM entries, which is what actually produced that orphan.
func cmdRetire(ctx context.Context, args []string, profile CorpusProfile) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: kb retire [--corpus homelab|ai] <id>")
		os.Exit(1)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid id %q: expected a positive integer\n", args[0])
		os.Exit(1)
	}

	title, found, err := lookupEntryTitle(ctx, profile, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading entry %d: %v\n", id, err)
		os.Exit(1)
	}
	if found {
		fmt.Fprintf(os.Stderr, "retire %s:%d — %s\n", profile.Name, id, title)
	} else {
		fmt.Fprintf(os.Stderr, "retire %s:%d — no SQLite row; continuing in case a Chroma vector is orphaned\n", profile.Name, id)
	}

	// Confirm only when a human is watching. Scripts and the MCP server run
	// without a terminal and must not block, but an interactive typo should not
	// be able to delete the wrong entry in one keystroke.
	if stdinIsTerminal() {
		fmt.Fprint(os.Stderr, "Delete permanently? [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(os.Stderr, "Aborted.")
			os.Exit(1)
		}
	}

	argv := profile.compileArgv("--retire", strconv.FormatInt(id, 10))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "retire failed (%s): %v\n", strings.Join(argv, " "), err)
		os.Exit(1)
	}
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// slugify transliterates Serbian Latin and Cyrillic characters to ASCII and produces a URL-safe slug.
func slugify(s string) string {
	s = transliterateSerbian(strings.ToLower(s))
	var result strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			result.WriteRune(r)
			lastDash = false
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
			// Unrecognized unicode letter — replace with dash to preserve word boundary
			if !lastDash {
				result.WriteRune('-')
				lastDash = true
			}
		} else if !lastDash {
			result.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(result.String(), "-")
	if len(out) > 60 {
		runes := []rune(out)
		if len(runes) > 60 {
			out = string(runes[:60])
		}
	}
	if out == "" {
		out = "untitled"
	}
	return out
}

// transliterateSerbian maps Serbian Latin and Cyrillic characters to their ASCII equivalents.
func transliterateSerbian(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if rep, ok := serbianMap[r]; ok {
			sb.WriteString(rep)
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var serbianMap = map[rune]string{
	// Latin diacritics (uppercase → uppercase ASCII, lowercase → lowercase ASCII)
	'š': "s", 'Š': "S",
	'č': "c", 'Č': "C",
	'ć': "c", 'Ć': "C",
	'ž': "z", 'Ž': "Z",
	'đ': "dj", 'Đ': "Dj",
	// Cyrillic (Serbian)
	'а': "a", 'А': "a", 'б': "b", 'Б': "b", 'в': "v", 'В': "v",
	'г': "g", 'Г': "g", 'д': "d", 'Д': "d", 'ђ': "dj", 'Ђ': "dj",
	'е': "e", 'Е': "e", 'ж': "z", 'Ж': "z", 'з': "z", 'З': "z",
	'и': "i", 'И': "i", 'ј': "j", 'Ј': "j", 'к': "k", 'К': "k",
	'л': "l", 'Л': "l", 'љ': "lj", 'Љ': "lj", 'м': "m", 'М': "m",
	'н': "n", 'Н': "n", 'њ': "nj", 'Њ': "nj", 'о': "o", 'О': "o",
	'п': "p", 'П': "p", 'р': "r", 'Р': "r", 'с': "s", 'С': "s",
	'т': "t", 'Т': "t", 'ћ': "c", 'Ћ': "c", 'у': "u", 'У': "u",
	'ф': "f", 'Ф': "f", 'х': "h", 'Х': "h", 'ц': "c", 'Ц': "c",
	'ч': "c", 'Ч': "c", 'џ': "dz", 'Џ': "dz", 'ш': "s", 'Ш': "s",
}

func firstLine(s string, maxLen int) string {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", 2)
	out := strings.TrimSpace(lines[0])
	if utf8.RuneCountInString(out) > maxLen {
		runes := []rune(out)
		out = string(runes[:maxLen]) + "..."
	}
	if out == "" {
		out = "Untitled"
	}
	return out
}

// truncate is a rune-safe string shortener (replaces the former truncateASCII).
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

// yamlQuote returns a JSON string literal, which is also a valid YAML scalar.
// Always quoting avoids YAML's implicit booleans/numbers and control-character edge cases.
func yamlQuote(s string) string {
	quoted, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(quoted)
}

// parseFTSQuery treats every whitespace-delimited term as a literal FTS5 phrase.
// This preserves implicit AND semantics while making IPs, paths, apostrophes, and
// operator words safe. Advanced FTS syntax is intentionally not accepted here.
func parseFTSQuery(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		escaped := strings.ReplaceAll(field, `"`, `""`)
		quoted = append(quoted, `"`+escaped+`"`)
	}
	return strings.Join(quoted, " ")
}

// alignOwnerWithDir gives a freshly created path the uid/gid of its parent
// directory. It is a no-op unless we run as root inside a corpus owned by
// someone else — the sudo case. Without it a sudo-run `kb add` leaves
// root-owned 0600 files in a user-owned corpus, and every later non-root
// reader fails on them: that is how kb-backup.sh died on 2026-08-06.
func alignOwnerWithDir(dir, path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid == 0 && stat.Gid == 0) {
		return nil
	}
	return os.Chown(path, int(stat.Uid), int(stat.Gid))
}

// writeRawFileExclusive creates a private raw file without ever overwriting an
// existing entry. O_EXCL is the collision guarantee; the nanosecond suffix only
// keeps the common path to a single attempt.
func writeRawFileExclusive(dir, prefix string, data []byte) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		suffix := time.Now().UnixNano()
		path := fmt.Sprintf("%s/%s-%d.md", dir, prefix, suffix)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}

		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "warn: could not remove incomplete file %s: %v\n", path, removeErr)
			}
			if writeErr != nil {
				return "", writeErr
			}
			return "", closeErr
		}
		if chownErr := alignOwnerWithDir(dir, path); chownErr != nil {
			fmt.Fprintf(os.Stderr, "warn: could not align owner of %s with %s: %v\n", path, dir, chownErr)
		}
		return path, nil
	}
	return "", fmt.Errorf("could not allocate unique raw filename after 100 attempts")
}

// openDB opens SQLite with WAL and busy_timeout (prevents "database is locked" errors).
func openDB(profile CorpusProfile) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", profile.sqliteDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// ─── OpenRouter ───────────────────────────────────────────────────────────────

func callOpenRouter(ctx context.Context, prompt string, start time.Time) error {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENROUTER_API_KEY not set")
	}

	modelToUse := os.Getenv("OPENROUTER_MODEL")
	if modelToUse == "" {
		modelToUse = defaultModel
	}

	fmt.Fprintf(os.Stderr, "[%v] Calling OpenRouter (%s)...\n",
		time.Since(start).Round(time.Millisecond), modelToUse)

	body, err := json.Marshal(map[string]any{
		"model": modelToUse,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"stream": true,
	})
	if err != nil {
		return fmt.Errorf("json marshal: %v", err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, "POST", openRouterURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpSlow.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			retryAfter := 2 * time.Second
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if sec, perr := strconv.Atoi(ra); perr == nil && sec > 0 {
					retryAfter = time.Duration(sec) * time.Second
				}
			}
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "[%v] 429 rate limited, retrying after %v...\n",
				time.Since(start).Round(time.Millisecond), retryAfter)
			select {
			case <-time.After(retryAfter):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("API error %d: %s", resp.StatusCode, string(b))
		}

		fmt.Fprintf(os.Stderr, "[%v] Stream started...\n", time.Since(start).Round(time.Millisecond))
		return streamCompletion(ctx, resp.Body, start)
	}
	return fmt.Errorf("OpenRouter rate limited after retry")
}

// streamCompletion prints the SSE stream from OpenRouter. Chunk is declared inside
// the loop to avoid the reuse bug (previous content value would repeat on empty delta).
func streamCompletion(ctx context.Context, body io.Reader, start time.Time) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	sawDone := false

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid SSE data: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("stream error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			fmt.Print(chunk.Choices[0].Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawDone {
		return fmt.Errorf("OpenRouter stream ended before [DONE]")
	}
	fmt.Println()
	return nil
}

// ─── formatting ───────────────────────────────────────────────────────────────

func formatResults(results []Result) string {
	if len(results) == 0 {
		return "(No relevant KB results found for this query.)"
	}
	var sb strings.Builder
	for _, r := range results {
		title := r.Title
		if title == "" {
			title = "Untitled"
		}
		sb.WriteString(fmt.Sprintf("### %s [%s] (%s) — %s\n", title, r.Source, r.Date, relevanceLabel(r.Score)))
		if r.Summary != "" {
			sb.WriteString(fmt.Sprintf("Summary: %s\n", r.Summary))
		}
		if r.Tags != "" {
			sb.WriteString(fmt.Sprintf("Tags: %s\n", r.Tags))
		}
		sb.WriteString(truncate(r.Content, 3000))
		sb.WriteString("\n\n---\n\n")
	}
	return sb.String()
}

// relevanceLabel maps a final_score (relevance × decay) to a qualitative band
// so the LLM can weigh evidence without knowing the numeric scale.
func relevanceLabel(score float64) string {
	switch {
	case score >= 0.60:
		return "HIGH relevance"
	case score >= 0.30:
		return "MEDIUM relevance"
	default:
		return "LOW relevance"
	}
}

// ─── env loading ──────────────────────────────────────────────────────────────

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := parseEnvLine(line)
		if !ok {
			continue
		}
		// Existing env takes precedence — do not override.
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		os.Setenv(key, val)
	}
}

// parseEnvLine parses a single .env file line. Returns (key, val, ok).
// Supports: KEY=value, KEY="value", KEY='value', export KEY=value.
// Skips: empty lines, comments (#), lines without =.
func parseEnvLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
	}
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key = strings.TrimSpace(parts[0])
	val = strings.TrimSpace(parts[1])
	if (len(val) >= 2) &&
		((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
		val = val[1 : len(val)-1]
	}
	if key == "" {
		return "", "", false
	}
	return key, val, true
}
