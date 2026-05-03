package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeSlug(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 80)
	expectedLong := strings.Repeat("a", 60)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ascii", "Fix the parser", "fix-the-parser"},
		{"cjk", "修复 解析器", "issue"},
		{"emoji", "📦 update", "update"},
		{"all-punct", "!!!", "issue"},
		{"leading-dash", "-foo", "foo"},
		{"trailing-dash", "foo-", "foo"},
		{"long", long, expectedLong},
		{"empty", "", "issue"},
		{"mixed-case", "FixIt NOW", "fixit-now"},
		{"keeps-dot-underscore", "v1.2_beta", "v1.2_beta"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeSlug(tc.in)
			if got != tc.want {
				t.Fatalf("SanitizeSlug(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBranchName(t *testing.T) {
	t.Parallel()
	got := BranchName(123, SanitizeSlug("Fix the parser"))
	want := "symphony/issue-123-fix-the-parser"
	if got != want {
		t.Fatalf("BranchName = %q; want %q", got, want)
	}
}

func TestLayoutFor(t *testing.T) {
	t.Parallel()
	root := filepath.Join("/tmp", "wt")
	got := LayoutFor(root, 7, "slug")
	want := Layout{
		Root:     filepath.Join(root, "issue-7-slug"),
		RepoPath: filepath.Join(root, "issue-7-slug", "repo"),
		HomePath: filepath.Join(root, "issue-7-slug", "home"),
		TmpPath:  filepath.Join(root, "issue-7-slug", "home", "tmp"),
	}
	if got != want {
		t.Fatalf("LayoutFor mismatch:\n got %#v\nwant %#v", got, want)
	}
}

// initTestRepo creates a tiny git repo with a single commit on `main`,
// configured so it can stand in for a remote (init.defaultBranch may
// differ; we explicitly create main).
func initTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// Use init -b main where supported; older git might not have -b.
	cmd := exec.Command("git", "init", "-b", "main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Fallback for older git.
		cmd = exec.Command("git", "init", dir)
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			t.Fatalf("git init: %v / %v\n%s\n%s", err, err2, out, out2)
		}
		run("checkout", "-b", "main")
	}
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")

	// Create a bare clone to act as origin and reconfigure dir to use it.
	originDir := t.TempDir()
	originPath := filepath.Join(originDir, "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", dir, originPath).CombinedOutput(); err != nil {
		t.Fatalf("clone bare: %v\n%s", err, out)
	}
	run("remote", "add", "origin", originPath)
	run("fetch", "origin")
	// Set up tracking so origin/main exists locally.
	return dir
}

func TestManagerCreate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	repo := initTestRepo(t)
	wtRoot := t.TempDir()

	mgr := NewManager(repo)
	layout := LayoutFor(wtRoot, 1, "demo")
	branch := BranchName(1, "demo")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.Create(ctx, layout, "main", branch); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Worktree directory and a tracked file exist.
	if _, err := os.Stat(filepath.Join(layout.RepoPath, "README.md")); err != nil {
		t.Fatalf("expected README.md in worktree: %v", err)
	}
	if _, err := os.Stat(layout.HomePath); err != nil {
		t.Fatalf("home not created: %v", err)
	}
	if _, err := os.Stat(layout.TmpPath); err != nil {
		t.Fatalf("tmp not created: %v", err)
	}

	// Branch should exist locally.
	out, err := exec.Command("git", "-C", repo, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput()
	if err != nil {
		t.Fatalf("branch missing: %v\n%s", err, out)
	}

	// Second Create with same branch should error.
	layout2 := LayoutFor(wtRoot, 2, "demo")
	if err := mgr.Create(ctx, layout2, "main", branch); err == nil {
		t.Fatalf("expected error creating same branch twice")
	}
}

func TestIsClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	repo := initTestRepo(t)
	wtRoot := t.TempDir()
	mgr := NewManager(repo)
	layout := LayoutFor(wtRoot, 5, "clean")
	branch := BranchName(5, "clean")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mgr.Create(ctx, layout, "main", branch); err != nil {
		t.Fatalf("Create: %v", err)
	}

	clean, err := mgr.IsClean(ctx, layout.RepoPath)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if !clean {
		t.Fatalf("expected clean worktree")
	}

	// Add an untracked file.
	if err := os.WriteFile(filepath.Join(layout.RepoPath, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err = mgr.IsClean(ctx, layout.RepoPath)
	if err != nil {
		t.Fatalf("IsClean (dirty): %v", err)
	}
	if clean {
		t.Fatalf("expected dirty worktree")
	}
}

func TestRunHookTimeout(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	ctx := context.Background()
	cwd := t.TempDir()
	res, err := RunHook(ctx, "sleep", "sleep 1", cwd, cwd, []string{"PATH=" + os.Getenv("PATH")}, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("RunHook err: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %#v", res)
	}
}

func TestRunHookHome(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	ctx := context.Background()
	cwd := t.TempDir()
	home := t.TempDir()
	res, err := RunHook(ctx, "home", `printf '%s' "$HOME"`, cwd, home, []string{"PATH=" + os.Getenv("PATH")}, 5*time.Second)
	if err != nil {
		t.Fatalf("RunHook err: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("unexpected timeout")
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, stderr=%s", res.ExitCode, res.Stderr)
	}
	if res.Stdout != home {
		t.Fatalf("stdout = %q; want %q", res.Stdout, home)
	}
}

func TestRunHookExitCode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	ctx := context.Background()
	cwd := t.TempDir()
	res, err := RunHook(ctx, "exit7", "exit 7", cwd, cwd, []string{"PATH=" + os.Getenv("PATH")}, 5*time.Second)
	if err != nil {
		t.Fatalf("RunHook err: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("unexpected timeout")
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d; want 7", res.ExitCode)
	}
}
