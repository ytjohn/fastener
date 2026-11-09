package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ytjohn/fastener/internal/claudetrust"
	"github.com/ytjohn/fastener/internal/config"
)

// authorizeKind is the only kind of authorization so far.
const authorizeKind = "claude-folder-trust"

const authorizeUsage = `usage: fastener authorize claude-folder-trust <dir> [--json]
       fastener authorize --list [--json]
       fastener authorize --revoke <dir> [--json]

claude-folder-trust marks <dir> as trusted for Claude Code, so that
"fastener run claude --remote --cwd <dir>" (claude remote-control) can start
there without anyone accepting Claude's interactive trust dialog first.

It edits Claude Code's own config file ($CLAUDE_CONFIG_DIR/.claude.json, else
~/.claude.json), setting projects[<key>].hasTrustDialogAccepted = true, which
is what accepting the dialog does and what Claude's own messages name as the
alternative to it. <key> is computed as Claude computes it: the root of the
git repository <dir> is in (for a linked worktree, the main repository's
root), else <dir> itself, so trusting a folder inside a repository trusts the
whole repository. The write takes Claude's lock (<config>.lock), re-reads the
file under it, changes only that one field and replaces the file atomically
(its mode is kept; the new file is owned by you); a backup goes to
<fastener config>/backups (the last 5 are kept). If Claude would use another
file (a non-empty legacy .config.json, or CLAUDE_CODE_CUSTOM_OAUTH_URL set),
the command refuses and names it.

Running this command is the explicit decision to trust <dir>. It is recorded
in Fastener's allowlist (authorizations.json in the Fastener config
directory), which lists what you have authorized; the allowlist is a record,
not a separate approval step. Nothing else in Fastener ever writes Claude's
config. The home directory and / are refused.

--list shows the allowlist: the recorded authorizations and config.toml's
trust_dirs. --revoke removes a folder from the allowlist and its record; it
does not change Claude's config (Claude keeps trusting the folder until you
remove projects[<key>] there yourself).

flags:
`

type authorizeOut struct {
	Folder         string `json:"folder"`
	Key            string `json:"key"`
	KeyIsRepoRoot  bool   `json:"key_is_repo_root"`
	ClaudeConfig   string `json:"claude_config"`
	AlreadyTrusted bool   `json:"already_trusted"`
	Backup         string `json:"backup,omitempty"`
	ClaudeVersion  string `json:"claude_version,omitempty"`
}

type authorizeListOut struct {
	SchemaVersion  int                    `json:"schema_version"`
	Authorizations []config.Authorization `json:"authorizations"`
	TrustDirs      []string               `json:"trust_dirs"`
}

type revokeOut struct {
	Folder       string `json:"folder"`
	Revoked      bool   `json:"revoked"`
	InTrustDirs  bool   `json:"in_trust_dirs"`
	Key          string `json:"key,omitempty"`
	ClaudeConfig string `json:"claude_config,omitempty"`
	// SameKey lists other recorded folders whose Claude key is the same.
	SameKey []string `json:"same_key,omitempty"`
}

func cmdAuthorize(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("authorize", stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), authorizeUsage)
		fs.PrintDefaults()
	}
	list := fs.Bool("list", false, "list the trust allowlist")
	revoke := fs.String("revoke", "", "remove `dir` from the allowlist (Claude's config is not changed)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	var revokeSet bool
	fs.Visit(func(f *flag.Flag) { revokeSet = revokeSet || f.Name == "revoke" })
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "fastener: %s\n%s", msg, strings.SplitN(authorizeUsage, "\n\n", 2)[0]+"\n")
		return exitUsage
	}
	switch {
	case *list && revokeSet:
		return usage("--list and --revoke are mutually exclusive")
	case *list:
		if len(pos) != 0 {
			return usage("--list takes no arguments")
		}
		return authorizeList(stdout, stderr, *asJSON)
	case revokeSet:
		if *revoke == "" {
			return usage("--revoke needs a folder")
		}
		if len(pos) != 0 {
			return usage("--revoke takes no other arguments")
		}
		return authorizeRevoke(*revoke, stdout, stderr, *asJSON)
	}
	if len(pos) == 0 {
		return usage("missing what to authorize (claude-folder-trust)")
	}
	if pos[0] != authorizeKind {
		return usage(fmt.Sprintf("unknown authorization %q (want %s)", pos[0], authorizeKind))
	}
	if len(pos) != 2 {
		return usage("claude-folder-trust takes exactly one folder")
	}
	return authorizeFolder(pos[1], stdout, stderr, *asJSON)
}

func authorizeFolder(dir string, stdout, stderr io.Writer, asJSON bool) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(stderr, err)
	}
	if config.Dir() == "" {
		return fail(stderr, config.ErrNoConfigDir)
	}
	folder, err := claudetrust.ResolveFolder(dir)
	if err != nil {
		return fail(stderr, err)
	}
	key, inRepo, err := claudetrust.Key(folder)
	if err != nil {
		return fail(stderr, err)
	}
	claudeCfg, err := claudetrust.ConfigPath()
	if err != nil {
		return fail(stderr, err)
	}
	if _, err := filepath.EvalSymlinks(claudeCfg); err != nil {
		return fail(stderr, fmt.Errorf("%s: %w", claudeCfg, claudetrust.ErrNoClaudeConfig))
	}

	// Running this command is the explicit act that allowlists the folder.
	now := time.Now().UTC().Truncate(time.Second)
	err = config.UpdateAuthorizations(func(recs []config.Authorization) ([]config.Authorization, error) {
		return upsertAuthorization(recs, config.Authorization{
			Folder: folder, Key: key, ClaudeConfig: claudeCfg, AuthorizedAt: now,
		}), nil
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("recording the authorization: %w", err))
	}
	// Claude's config is only ever written for an allowlisted folder.
	recs, err := config.LoadAuthorizations()
	if err != nil {
		return fail(stderr, err)
	}
	if !cfg.Allowlisted(folder, recs) {
		return fail(stderr, fmt.Errorf("%s is not on the trust allowlist", folder))
	}

	res, err := claudetrust.Trust(key, claudetrust.Options{
		ConfigPath: claudeCfg,
		BackupDir:  filepath.Join(config.Dir(), "backups"),
	})
	if err != nil {
		fmt.Fprintf(stderr, "fastener: %s stays on the allowlist; run the command again to retry\n", folder)
		return fail(stderr, err)
	}
	ver := claudeVersion()
	verified := time.Now().UTC().Truncate(time.Second)
	err = config.UpdateAuthorizations(func(recs []config.Authorization) ([]config.Authorization, error) {
		for i := range recs {
			if recs[i].Folder == folder {
				recs[i].VerifiedAt, recs[i].ClaudeVersion = &verified, ver
			}
		}
		return recs, nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "fastener: warning: Claude's config is updated, but recording that failed: %v\n", err)
	}

	out := authorizeOut{Folder: folder, Key: key, KeyIsRepoRoot: inRepo, ClaudeConfig: claudeCfg,
		AlreadyTrusted: res.AlreadyTrusted, Backup: res.Backup, ClaudeVersion: ver}
	if asJSON {
		writeJSON(stdout, out)
		return exitOK
	}
	if res.AlreadyTrusted {
		fmt.Fprintf(stdout, "already trusted: %s (nothing written)\n", folder)
	} else {
		fmt.Fprintf(stdout, "trusted %s for Claude Code\n", folder)
	}
	fmt.Fprintf(stdout, "  folder: %s\n", folder)
	keyNote := "(the folder itself)"
	if inRepo {
		keyNote = "(the git repository root: the whole repository is trusted)"
		if key == folder {
			keyNote = "(the folder is a git repository root)"
		}
	}
	fmt.Fprintf(stdout, "  key:    %s %s\n", key, keyNote)
	fmt.Fprintf(stdout, "  config: %s\n", claudeCfg)
	if res.Backup != "" {
		fmt.Fprintf(stdout, "  backup: %s\n", res.Backup)
	}
	return exitOK
}

// upsertAuthorization replaces the record for a.Folder, or appends a.
func upsertAuthorization(recs []config.Authorization, a config.Authorization) []config.Authorization {
	for i := range recs {
		if recs[i].Folder == a.Folder {
			recs[i] = a
			return recs
		}
	}
	return append(recs, a)
}

// claudeVersion is `claude --version`, or "" if it cannot be run.
func claudeVersion() string {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return truncate(strings.TrimSpace(string(out)), 200)
}

func authorizeList(stdout, stderr io.Writer, asJSON bool) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(stderr, err)
	}
	recs, err := config.LoadAuthorizations()
	if err != nil {
		return fail(stderr, err)
	}
	if recs == nil {
		recs = []config.Authorization{}
	}
	dirs := append([]string{}, cfg.TrustDirs...)
	if asJSON {
		writeJSON(stdout, authorizeListOut{SchemaVersion: JSONSchemaVersion, Authorizations: recs, TrustDirs: dirs})
		return exitOK
	}
	if len(recs) == 0 && len(dirs) == 0 {
		fmt.Fprintln(stdout, "no folders are authorized")
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "FOLDER\tKEY\tAUTHORIZED\tVERIFIED\tCLAUDE\tCONFIG")
	for _, r := range recs {
		verified, ver := "no", r.ClaudeVersion
		if r.VerifiedAt != nil {
			verified = r.VerifiedAt.Format(time.RFC3339)
		}
		if ver == "" {
			ver = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Folder, r.Key, r.AuthorizedAt.Format(time.RFC3339), verified, ver, r.ClaudeConfig)
	}
	tw.Flush()
	if len(dirs) > 0 {
		fmt.Fprintf(stdout, "\ntrust_dirs in %s:\n", filepath.Join(config.Dir(), "config.toml"))
		for _, d := range dirs {
			fmt.Fprintf(stdout, "  %s\n", d)
		}
	}
	return exitOK
}

func authorizeRevoke(dir string, stdout, stderr io.Writer, asJSON bool) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(stderr, err)
	}
	// The folder may be gone by now, so match both its resolved and its
	// cleaned absolute path.
	cands := map[string]bool{}
	if abs, err := filepath.Abs(dir); err == nil {
		cands[abs] = true
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			cands[real] = true
		}
	}
	var removed []config.Authorization
	err = config.UpdateAuthorizations(func(recs []config.Authorization) ([]config.Authorization, error) {
		kept := recs[:0]
		for _, r := range recs {
			if cands[r.Folder] {
				removed = append(removed, r)
				continue
			}
			kept = append(kept, r)
		}
		return kept, nil
	})
	if err != nil {
		return fail(stderr, err)
	}
	inDirs := false
	for c := range cands {
		inDirs = inDirs || cfg.InTrustDirs(c)
	}
	var sharing []string // recorded folders that still use the same Claude key
	if len(removed) > 0 {
		if recs, err := config.LoadAuthorizations(); err == nil {
			for _, r := range recs {
				if r.Key == removed[0].Key {
					sharing = append(sharing, r.Folder)
				}
			}
		}
	}
	out := revokeOut{Folder: dir, Revoked: len(removed) > 0, InTrustDirs: inDirs, SameKey: sharing}
	if len(removed) > 0 {
		out.Folder, out.Key, out.ClaudeConfig = removed[0].Folder, removed[0].Key, removed[0].ClaudeConfig
	}
	if inDirs {
		fmt.Fprintf(stderr, "fastener: %s is still listed in trust_dirs in %s; remove it there to take it off the allowlist\n",
			dir, filepath.Join(config.Dir(), "config.toml"))
	}
	if len(removed) == 0 {
		if asJSON {
			writeJSON(stdout, out)
		}
		if inDirs {
			return fail(stderr, errors.New(dir+" has no authorization record to revoke"))
		}
		return fail(stderr, errors.New(dir+" is not authorized"))
	}
	if asJSON {
		writeJSON(stdout, out)
		return exitOK
	}
	fmt.Fprintf(stdout, "revoked %s: removed from Fastener's allowlist and records\n", out.Folder)
	if len(sharing) > 0 {
		fmt.Fprintf(stdout, "still authorized with the same Claude key %s (removing that entry from Claude's config would untrust them too): %s\n", out.Key, strings.Join(sharing, ", "))
	}
	fmt.Fprintf(stdout, "Claude's config was not changed: %s still has projects[%q].%s; remove that entry yourself to make Claude ask again\n",
		out.ClaudeConfig, out.Key, claudetrust.TrustField)
	return exitOK
}
