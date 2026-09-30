package main

import (
	"strings"
	"testing"
)

// every filter must be non-destructive on the facts it claims to keep:
// branch, counts, file paths, line numbers, error text.
func TestFilterFidelity(t *testing.T) {
	// git diff: every changed line must be present, or counted
	diff := "diff --git a/main.go b/main.go\nindex 111..222 100644\n--- a/main.go\n+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n package main\n \n+// added line\n func main() {\n-// removed line\n }\n"
	out := rtkGitDiff(diff)
	for _, want := range []string{"main.go", "// added line", "func main()", "+1 -1"} {
		if !strings.Contains(out, want) {
			t.Errorf("git-diff lost %q:\n%s", want, out)
		}
	}

	// a large hunk must truncate but still report what it dropped
	var sb strings.Builder
	sb.WriteString("diff --git a/big.go b/big.go\n--- a/big.go\n+++ b/big.go\n@@ -1,300 +1,300 @@\n")
	for i := 0; i < 300; i++ {
		sb.WriteString("+added line number ")
		sb.WriteString(strings.Repeat("x", 3))
		sb.WriteString(itoa(i))
		sb.WriteString("\n")
	}
	big := rtkGitDiff(sb.String())
	if !strings.Contains(big, "truncated") {
		t.Errorf("large hunk not marked truncated:\n%s", big[:200])
	}
	if !strings.Contains(big, "+300 -0") {
		t.Errorf("hunk counts lost:\n%s", big[:300])
	}

	// build output: every error must survive, progress must not
	build := "Compiling foo v1.0\nCompiling bar v2.0\nCompiling baz v3.0\n" +
		"error[E0308]: mismatched types\n --> src/main.rs:10:5\n  |\n10 |     let x: i32 = \"s\";\n" +
		"warning: unused variable `y`\n  --> src/lib.rs:3:9\n" +
		"Finished dev [unoptimized] target(s)\n"
	bout := rtkBuildOutput(build)
	if !strings.Contains(bout, "E0308") || !strings.Contains(bout, "src/main.rs:10:5") ||
		!strings.Contains(bout, "mismatched types") {
		t.Errorf("build-output lost the error:\n%s", bout)
	}
	if !strings.Contains(bout, "unused variable") {
		t.Errorf("build-output lost the warning:\n%s", bout)
	}
	if !strings.Contains(bout, "Compiled 3 packages") {
		t.Errorf("build-output lost the compile count:\n%s", bout)
	}
	if strings.Contains(bout, "Compiling foo v1.0") {
		t.Errorf("build-output kept raw progress lines:\n%s", bout)
	}

	// grep: every file must appear, and the per-file cap must be honest
	var g strings.Builder
	for i := 0; i < 30; i++ {
		g.WriteString("src/f" + itoa(i) + ".go:" + itoa(i) + ":  match in file " + itoa(i) + "\n")
	}
	gout := rtkGrep(g.String())
	if !strings.Contains(gout, "30 matches in 30F") {
		t.Errorf("grep lost the total:\n%s", gout)
	}
	for i := 0; i < 30; i++ {
		if !strings.Contains(gout, "src/f"+itoa(i)+".go") {
			t.Fatalf("grep dropped file %d", i)
		}
	}
	// the per-file cap is 10: one file with 25 matches shows 10 and says so
	var one strings.Builder
	for i := 0; i < 25; i++ {
		one.WriteString("src/one.go:" + itoa(i) + ":  hit " + itoa(i) + "\n")
	}
	ocap := rtkGrep(one.String())
	if !strings.Contains(ocap, "[file] src/one.go (25)") {
		t.Errorf("grep lost the per-file total:\n%s", ocap)
	}
	if !strings.Contains(ocap, "+15") {
		t.Errorf("grep did not report the cap honestly:\n%s", ocap)
	}
	if strings.Contains(ocap, "hit 24") {
		t.Errorf("grep showed past the cap:\n%s", ocap)
	}

	// ls: noise dirs are the one intentional omission; real files must survive
	lsIn := "total 24\n" +
		"drwxr-xr-x  4 me  staff  128 Jan  2 10:00 node_modules\n" +
		"drwxr-xr-x  4 me  staff  128 Jan  2 10:00 src\n" +
		"-rw-r--r--  1 me  staff  2048 Jan  2 10:00 main.go\n" +
		"-rw-r--r--  1 me  staff  1024 Jan  2 10:00 README.md\n"
	lout := rtkLs(lsIn)
	if !strings.Contains(lout, "main.go") || !strings.Contains(lout, "README.md") {
		t.Errorf("ls dropped real files:\n%s", lout)
	}
	if !strings.Contains(lout, "src/") {
		t.Errorf("ls dropped a real dir:\n%s", lout)
	}
	if strings.Contains(lout, "node_modules") {
		t.Errorf("ls kept a noise dir:\n%s", lout)
	}
	if !strings.Contains(lout, "Summary: 2 files, 1 dirs") {
		t.Errorf("ls summary wrong:\n%s", lout)
	}

	// git log: subjects and hashes must survive
	log := "commit abc1234567890abcdef1234567890abcdef12\nAuthor: A <a@b.c>\nDate:   Mon Jan 2\n\n    fix the thing\n\n    long body that should be dropped entirely\n" +
		"commit def1234567890abcdef1234567890abcdef12\nAuthor: B <b@b.c>\nDate:   Tue Jan 3\n\n    another fix\n"
	l2 := rtkGitLog(log)
	for _, want := range []string{"abc1234567890abcdef1234567890abcdef12", "fix the thing", "def1234567890abcdef1234567890abcdef12", "another fix"} {
		if !strings.Contains(l2, want) {
			t.Errorf("git-log lost %q:\n%s", want, l2)
		}
	}
	if strings.Contains(l2, "long body that should be dropped") {
		t.Errorf("git-log kept the body:\n%s", l2)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
