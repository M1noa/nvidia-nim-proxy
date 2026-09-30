package main

// RTK port: compress tool_result content in LLM request bodies before it goes
// upstream. Go port of 9router's open-sse/rtk/ (itself a port of rtk-ai/rtk).
//
// fail-open: a filter that misbehaves returns its input, and compressText
// never returns empty output or output larger than its input. tool results
// marked is_error are skipped so failure traces survive intact.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	rtkMinCompressSize = 500      // bytes; skip tiny blobs
	rtkRawCap          = 10 << 20 // 10 MiB
	rtkDetectWindow    = 1024     // autodetect peeks first N chars
	rtkDiffHunkMax     = 100      // per-hunk line cap
	rtkLogMaxLines     = 200
	rtkDedupLineMax    = 2000
	rtkGrepPerFileMax  = 10
	rtkFindPerDirMax   = 10
	rtkFindTotalDirMax = 20
	rtkStatusMaxFiles  = 10
	rtkStatusMaxUntrac = 10
	rtkTreeMaxLines    = 200
	rtkSearchPerDirMax = 10
	rtkSearchDirMax    = 20
	rtkTruncHead       = 120 // lines kept from top
	rtkTruncTail       = 60  // lines kept from bottom
	rtkTruncMinLines   = 250 // only truncate above this
	rtkReadNumMinRatio = 0.7
	rtkDeprecationKeep = 3
	rtkWarningKeep     = 5
	rtkExtSummaryTop   = 5
)

var lsNoiseDirs = map[string]bool{
	"node_modules": true, ".git": true, "target": true, "__pycache__": true,
	".next": true, "dist": true, "build": true, ".cache": true, ".turbo": true,
	".vercel": true, ".pytest_cache": true, ".mypy_cache": true, ".tox": true,
	".venv": true, "venv": true, "env": true, "coverage": true, ".nyc_output": true,
	".DS_Store": true, "Thumbs.db": true, ".idea": true, ".vscode": true, ".vs": true,
	"*.egg-info": true, ".eggs": true,
}

// --- filters ---

// rtkGitLog keeps commit headers, author/date and subject, drops bodies.
func rtkGitLog(in string) string {
	lines := strings.Split(in, "\n")
	out := make([]string, 0, len(lines))
	skipped := 0
	inCommit := false
	subjectSeen := false

	push := func(l string) {
		if len(out) < rtkLogMaxLines {
			out = append(out, l)
			return
		}
		skipped++
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)

		if reCommitHdr.MatchString(trimmed) {
			inCommit = true
			subjectSeen = false
			push(line)
			continue
		}
		if inCommit {
			if reAuthorDate.MatchString(trimmed) {
				push(trimmed)
				continue
			}
			if trimmed == "" {
				continue
			}
			if !subjectSeen && reIndented.MatchString(line) {
				push("  Subject: " + trimmed)
				subjectSeen = true
				continue
			}
			if reFilesChanged.MatchString(trimmed) {
				push("  " + trimmed)
				continue
			}
			if strings.HasPrefix(trimmed, "diff --git ") {
				push("  ... diff body omitted")
			}
			continue
		}
		if reGraphEntry.MatchString(trimmed) {
			push(trimmed)
			continue
		}
		if reOneline.MatchString(trimmed) {
			push(trimmed)
			continue
		}
		if trimmed != "" && reGraphOnly.MatchString(trimmed) {
			continue
		}
		push(trimmed)
	}
	if skipped > 0 {
		out = append(out, "..."+strconv.Itoa(skipped)+" more lines")
	}
	return rtkShrink(in, strings.Join(out, "\n"))
}

// rtkGitDiff compacts unified diff: file headers, per-hunk line cap, +/- counts.
func rtkGitDiff(in string) string {
	result := make([]string, 0, 256)
	file, added, removed := "", 0, 0
	inHunk, hunkShown, hunkSkipped, truncated := false, 0, 0, false

	flushSkipped := func() {
		if hunkSkipped > 0 {
			result = append(result, "  ... ("+strconv.Itoa(hunkSkipped)+" lines truncated)")
			truncated = true
			hunkSkipped = 0
		}
	}

	for _, line := range strings.Split(in, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			flushSkipped()
			if file != "" && (added > 0 || removed > 0) {
				result = append(result, "  +"+strconv.Itoa(added)+" -"+strconv.Itoa(removed))
			}
			if i := strings.Index(line, " b/"); i >= 0 {
				file = line[i+3:]
			} else {
				file = "unknown"
			}
			result = append(result, "\n"+file)
			added, removed, inHunk, hunkShown = 0, 0, false, 0

		case strings.HasPrefix(line, "@@"):
			flushSkipped()
			inHunk, hunkShown = true, 0
			result = append(result, "  "+line)

		case inHunk:
			switch {
			case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
				added++
				if hunkShown < rtkDiffHunkMax {
					result = append(result, "  "+line)
					hunkShown++
				} else {
					hunkSkipped++
				}
			case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
				removed++
				if hunkShown < rtkDiffHunkMax {
					result = append(result, "  "+line)
					hunkShown++
				} else {
					hunkSkipped++
				}
			case hunkShown < rtkDiffHunkMax && !strings.HasPrefix(line, `\`):
				if hunkShown > 0 {
					result = append(result, "  "+line)
					hunkShown++
				}
			}
		}
		if len(result) >= 500 {
			result = append(result, "\n... (more changes truncated)")
			truncated = true
			goto done
		}
	}
done:
	flushSkipped()
	if file != "" && (added > 0 || removed > 0) {
		result = append(result, "  +"+strconv.Itoa(added)+" -"+strconv.Itoa(removed))
	}
	if truncated {
		result = append(result, "[full diff: rtk git diff --no-compact]")
	}
	return strings.Join(result, "\n")
}

// rtkGitStatus folds porcelain or long status output into grouped counts.
func rtkGitStatus(in string) string {
	lines := strings.Split(in, "\n")
	if len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		return "Clean working tree"
	}

	branch := ""
	var stagedF, modF, untrF []string
	staged, modified, untracked, conflicts := 0, 0, 0, 0
	inUntracked := false

	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if m := reOnBranch.FindStringSubmatch(raw); m != nil {
			branch = m[1]
			continue
		}
		if strings.HasPrefix(raw, "##") {
			branch = strings.TrimSpace(strings.TrimPrefix(raw, "##"))
			continue
		}
		if len(raw) >= 3 && rePorcelainPrefix.MatchString(raw) {
			x, y := raw[0], raw[1]
			file := raw[3:]
			if raw[:2] == "??" {
				untracked++
				untrF = append(untrF, file)
				continue
			}
			if strings.ContainsRune("MADRC", rune(x)) {
				staged++
				stagedF = append(stagedF, file)
			} else if x == 'U' {
				conflicts++
			}
			if y == 'M' || y == 'D' {
				modified++
				modF = append(modF, file)
			}
			continue
		}
		if m := reLongStatus.FindStringSubmatch(raw); m != nil {
			switch m[1] {
			case "both modified":
				conflicts++
			case "modified", "deleted":
				modified++
				modF = append(modF, strings.TrimSpace(m[2]))
			case "new file", "renamed":
				staged++
				stagedF = append(stagedF, strings.TrimSpace(m[2]))
			}
			continue
		}
		// long-form "Untracked files:" sections list bare tab-indented paths
		// after the marker. without this they are dropped and the summary
		// wrongly reports a clean tree.
		if inUntracked && strings.HasPrefix(raw, "\t") && strings.TrimSpace(raw) != "" {
			untracked++
			untrF = append(untrF, strings.TrimSpace(raw))
			continue
		}
		if reUntrackedHeader.MatchString(raw) {
			inUntracked = true
			continue
		}
	}

	var b strings.Builder
	if branch != "" {
		b.WriteString("* " + branch + "\n")
	}
	writeGroup := func(label string, n int, files []string, cap int) {
		if n == 0 {
			return
		}
		b.WriteString(label + " " + strconv.Itoa(n) + " files\n")
		for i, f := range files {
			if i >= cap {
				b.WriteString("   ... +" + strconv.Itoa(len(files)-cap) + " more\n")
				break
			}
			b.WriteString("   " + f + "\n")
		}
	}
	writeGroup("+ Staged:", staged, stagedF, rtkStatusMaxFiles)
	writeGroup("~ Modified:", modified, modF, rtkStatusMaxFiles)
	writeGroup("? Untracked:", untracked, untrF, rtkStatusMaxUntrac)
	if conflicts > 0 {
		b.WriteString("conflicts: " + strconv.Itoa(conflicts) + " files\n")
	}
	if staged == 0 && modified == 0 && untracked == 0 && conflicts == 0 {
		b.WriteString("clean - nothing to commit\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// rtkBuildOutput keeps errors, warnings and the final summary; drops progress.
func rtkBuildOutput(in string) string {
	lines := strings.Split(in, "\n")
	var errors, warnings, deprecations []string
	var summary string
	compiling, downloading := 0, 0
	inCargoErr := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inCargoErr {
			if trimmed == "" {
				inCargoErr = false
				continue
			}
			if reCargoErrCont.MatchString(line) {
				errors = append(errors, line)
				continue
			}
			inCargoErr = false
		}
		if trimmed == "" {
			continue
		}

		switch {
		case reNpmErr.MatchString(trimmed), reYarnErr.MatchString(trimmed):
			errors = append(errors, line)
		case reNpmDeprecated.MatchString(trimmed):
			deprecations = append(deprecations, line)
		case reNpmWarn.MatchString(trimmed), reYarnWarn.MatchString(trimmed):
			warnings = append(warnings, line)
		case reErrLabel.MatchString(trimmed):
			errors = append(errors, line)
			inCargoErr = true
		case reWarnLabel.MatchString(trimmed):
			warnings = append(warnings, line)
			inCargoErr = true
		case reErrUpper.MatchString(trimmed), reBrackErr.MatchString(trimmed):
			errors = append(errors, line)
		case reBrackWarn.MatchString(trimmed):
			warnings = append(warnings, line)
		case reCompiling.MatchString(trimmed):
			compiling++
		case reDownloading.MatchString(trimmed):
			downloading++
		case reBuildSummary.MatchString(trimmed):
			if summary != "" {
				summary += "\n" + line
			} else {
				summary = line
			}
		}
	}

	var b strings.Builder
	for i, d := range deprecations {
		if i >= rtkDeprecationKeep {
			b.WriteString("... +" + strconv.Itoa(len(deprecations)-rtkDeprecationKeep) + " more deprecated packages\n")
			break
		}
		b.WriteString(d + "\n")
	}
	if compiling > 0 {
		b.WriteString("Compiled " + strconv.Itoa(compiling) + " packages\n")
	}
	if downloading > 0 {
		b.WriteString("Downloaded " + strconv.Itoa(downloading) + " packages\n")
	}
	for _, e := range errors {
		b.WriteString(e + "\n")
	}
	for i, w := range warnings {
		if i >= rtkWarningKeep {
			b.WriteString("... +" + strconv.Itoa(len(warnings)-rtkWarningKeep) + " more warnings\n")
			break
		}
		b.WriteString(w + "\n")
	}
	if summary != "" {
		b.WriteString(summary + "\n")
	}
	if out := strings.TrimRight(b.String(), "\n"); out != "" {
		return out
	}
	return in
}

// rtkGrep groups "file:line:content" matches by file, capped per file.
func rtkGrep(in string) string {
	type pair struct{ num, content string }
	byFile := map[string][]pair{}
	order := []string{}
	total := 0

	for _, line := range strings.Split(in, "\n") {
		f1 := strings.IndexByte(line, ':')
		if f1 < 0 {
			continue
		}
		f2 := strings.IndexByte(line[f1+1:], ':')
		if f2 < 0 {
			continue
		}
		f2 += f1 + 1
		num := line[f1+1 : f2]
		if !isAllDigits(num) {
			continue
		}
		total++
		file := line[:f1]
		if _, ok := byFile[file]; !ok {
			order = append(order, file)
		}
		byFile[file] = append(byFile[file], pair{num, line[f2+1:]})
	}
	if total == 0 {
		return in
	}

	sort.Strings(order)
	var b strings.Builder
	b.WriteString(strconv.Itoa(total) + " matches in " + strconv.Itoa(len(order)) + "F:\n\n")
	for _, file := range order {
		ms := byFile[file]
		b.WriteString("[file] " + file + " (" + strconv.Itoa(len(ms)) + "):\n")
		for i, m := range ms {
			if i >= rtkGrepPerFileMax {
				b.WriteString("  +" + strconv.Itoa(len(ms)-rtkGrepPerFileMax) + "\n")
				break
			}
			num := m.num
			for len(num) < 4 {
				num = " " + num
			}
			b.WriteString("  " + num + ": " + strings.TrimSpace(m.content) + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// rtkFind groups path lists by parent dir, capped per dir and in total.
func rtkFind(in string) string {
	var paths []string
	for _, l := range strings.Split(in, "\n") {
		if strings.TrimSpace(l) != "" {
			paths = append(paths, l)
		}
	}
	if len(paths) == 0 {
		return in
	}

	byDir := map[string][]string{}
	for _, p := range paths {
		dir, name := ".", p
		if i := strings.LastIndexAny(p, `/\`); i >= 0 {
			dir = p[:i]
			if dir == "" {
				dir = "/"
			}
			name = p[i+1:]
		}
		byDir[dir] = append(byDir[dir], name)
	}

	dirs := sortedKeys(byDir)
	var b strings.Builder
	b.WriteString(strconv.Itoa(len(paths)) + " files in " + strconv.Itoa(len(dirs)) + " dirs:\n\n")
	for i, d := range dirs {
		if i >= rtkFindTotalDirMax {
			b.WriteString("\n+" + strconv.Itoa(len(dirs)-rtkFindTotalDirMax) + " more dirs\n")
			break
		}
		fs := byDir[d]
		b.WriteString(strings.ReplaceAll(d, `\`, "/") + "/  (" + strconv.Itoa(len(fs)) + ")\n")
		for j, f := range fs {
			if j >= rtkFindPerDirMax {
				b.WriteString("  +" + strconv.Itoa(len(fs)-rtkFindPerDirMax) + "\n")
				break
			}
			b.WriteString("  " + f + "\n")
		}
	}
	return b.String()
}

// rtkSearchList compacts Cursor's "Result of search in '...'" glob output.
func rtkSearchList(in string) string {
	lines := strings.Split(in, "\n")
	if len(lines) == 0 {
		return in
	}
	header := lines[0]

	var paths []string
	for _, raw := range lines[1:] {
		if t := strings.TrimSpace(raw); strings.HasPrefix(t, "- ") {
			paths = append(paths, strings.TrimSpace(t[2:]))
		}
	}
	if len(paths) == 0 {
		return in
	}

	byDir := map[string][]string{}
	for _, p := range paths {
		dir, name := ".", p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			dir = p[:i]
			if dir == "" {
				dir = "/"
			}
			name = p[i+1:]
		}
		byDir[dir] = append(byDir[dir], name)
	}

	dirs := sortedKeys(byDir)
	var b strings.Builder
	b.WriteString(header + "\n" + strconv.Itoa(len(paths)) + " files in " + strconv.Itoa(len(dirs)) + " dirs:\n\n")
	for i, d := range dirs {
		if i >= rtkSearchDirMax {
			b.WriteString("+" + strconv.Itoa(len(dirs)-rtkSearchDirMax) + " more dirs\n")
			break
		}
		ns := byDir[d]
		b.WriteString(d + "/ (" + strconv.Itoa(len(ns)) + "):\n")
		for j, n := range ns {
			if j >= rtkSearchPerDirMax {
				b.WriteString("  +" + strconv.Itoa(len(ns)-rtkSearchPerDirMax) + "\n")
				break
			}
			b.WriteString("  " + n + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// rtkDedupLog collapses consecutive duplicate lines, blank runs, and caps height.
func rtkDedupLog(in string) string {
	var out []string
	prev := ""
	hasPrev := false
	run := 0
	blank := 0

	flush := func() {
		if hasPrev && run > 1 {
			out = append(out, "  ... ("+strconv.Itoa(run-1)+" duplicate lines)")
		}
	}
	for _, line := range strings.Split(in, "\n") {
		if strings.TrimSpace(line) == "" {
			if blank < 1 {
				out = append(out, line)
			}
			blank++
			flush()
			hasPrev, run = false, 0
			continue
		}
		blank = 0
		if hasPrev && line == prev {
			run++
			continue
		}
		flush()
		out = append(out, line)
		prev, hasPrev, run = line, true, 1
		if len(out) >= rtkDedupLineMax {
			out = append(out, "... (truncated at "+strconv.Itoa(rtkDedupLineMax)+" lines)")
			return strings.Join(out, "\n")
		}
	}
	flush()
	return strings.Join(out, "\n")
}

// rtkLs compacts `ls -la` output, dropping noise dirs and summarising by ext.
func rtkLs(in string) string {
	var dirs, files []string
	byExt := map[string]int{}

	for _, line := range strings.Split(in, "\n") {
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		kind, size, name, ok := parseLsLine(line)
		if !ok || name == "." || name == ".." || lsNoiseDirs[name] {
			continue
		}
		if kind == 'd' {
			dirs = append(dirs, name)
		} else if kind == '-' || kind == 'l' {
			ext := "no ext"
			if i := strings.LastIndex(name, "."); i > 0 {
				ext = name[i:]
			}
			byExt[ext]++
			files = append(files, name+"  "+humanSize(size))
		}
	}
	if len(dirs) == 0 && len(files) == 0 {
		return in
	}

	var b strings.Builder
	for _, d := range dirs {
		b.WriteString(d + "/\n")
	}
	for _, f := range files {
		b.WriteString(f + "\n")
	}
	summary := "\nSummary: " + strconv.Itoa(len(files)) + " files, " + strconv.Itoa(len(dirs)) + " dirs"
	if len(byExt) > 0 {
		exts := sortedCounts(byExt)
		parts := make([]string, 0, rtkExtSummaryTop)
		for i, e := range exts {
			if i >= rtkExtSummaryTop {
				break
			}
			parts = append(parts, strconv.Itoa(e.n)+" "+e.k)
		}
		summary += " (" + strings.Join(parts, ", ")
		if len(exts) > rtkExtSummaryTop {
			summary += ", +" + strconv.Itoa(len(exts)-rtkExtSummaryTop) + " more"
		}
		summary += ")"
	}
	return b.String() + summary
}

// rtkTree drops tree's summary line and caps height.
func rtkTree(in string) string {
	var kept []string
	for _, line := range strings.Split(in, "\n") {
		if strings.Contains(line, "director") && strings.Contains(line, "file") {
			continue
		}
		if strings.TrimSpace(line) == "" && len(kept) == 0 {
			continue
		}
		kept = append(kept, line)
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	if len(kept) > rtkTreeMaxLines {
		cut := len(kept) - rtkTreeMaxLines
		return strings.Join(kept[:rtkTreeMaxLines], "\n") + "\n... +" + strconv.Itoa(cut) + " more lines"
	}
	return strings.Join(kept, "\n")
}

// rtkReadNumbered truncates Cursor/Codex "  N|content" file dumps.
func rtkReadNumbered(in string) string {
	return rtkHeadTail(in, "file continues")
}

// rtkSmartTruncate keeps head and tail of an unidentifiable large blob.
// gated behind rtk.blind_truncate; see compressText.
func rtkSmartTruncate(in string) string {
	return rtkHeadTail(in, "lines truncated")
}

func rtkHeadTail(in, marker string) string {
	lines := strings.Split(in, "\n")
	if len(lines) < rtkTruncMinLines {
		return in
	}
	head := lines[:rtkTruncHead]
	tail := lines[len(lines)-rtkTruncTail:]
	cut := len(lines) - len(head) - len(tail)
	out := make([]string, 0, len(head)+len(tail)+1)
	out = append(out, head...)
	out = append(out, "... +"+strconv.Itoa(cut)+" "+marker)
	return strings.Join(append(out, tail...), "\n")
}

// --- autodetect ---

// rtkFilter is a compressor. the name is carried in the type so the log
// line can report it; plain func values are not comparable in Go.
type rtkFilter struct {
	name string
	fn   func(string) string
}

func (f rtkFilter) call(s string) string { return f.fn(s) }

// rtkAutoDetect picks a filter for text, or nil when nothing matches. blind
// gates the last-resort truncation of unidentifiable large blobs.
func rtkAutoDetect(text string, blind bool) *rtkFilter {
	head := text
	if len(head) > rtkDetectWindow {
		head = head[:rtkDetectWindow]
	}
	lines := strings.Split(head, "\n")
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}

	switch {
	case reGitLog.MatchString(head):
		return &rtkFilter{"git-log", rtkGitLog}
	case reGitDiff.MatchString(head), reGitDiffHunk.MatchString(head):
		return &rtkFilter{"git-diff", rtkGitDiff}
	case reGitStatus.MatchString(head):
		return &rtkFilter{"git-status", rtkGitStatus}
	// build output before porcelain: cargo "Compiling" must not read as status
	case reBuildOut.MatchString(head):
		return &rtkFilter{"build-output", rtkBuildOutput}
	case rtkMostlyPorcelain(lines):
		return &rtkFilter{"git-status", rtkGitStatus}
	}

	// grep: any of the first 5 non-empty lines is "file:number:content"
	first5 := nonEmpty
	if len(first5) > 5 {
		first5 = first5[:5]
	}
	for _, l := range first5 {
		if isGrepLine(l) {
			return &rtkFilter{"grep", rtkGrep}
		}
	}

	// find: every non-empty line is path-like, at least 3 of them
	if len(nonEmpty) >= 3 && rtkAllPathLike(nonEmpty) {
		return &rtkFilter{"find", rtkFind}
	}

	if reTreeGlyph.MatchString(head) {
		return &rtkFilter{"tree", rtkTree}
	}
	if reLsTotal.MatchString(head) || rtkCountMatches(head, reLsRow) >= 3 {
		return &rtkFilter{"ls", rtkLs}
	}
	if reSearchHeader.MatchString(head) {
		return &rtkFilter{"search-list", rtkSearchList}
	}
	if len(lines) >= rtkTruncMinLines && rtkIsLineNumbered(lines) {
		return &rtkFilter{"read-numbered", rtkReadNumbered}
	}
	// dedup-log only earns its place when there is duplication to collapse.
	// without this check it claims every multi-line blob and then returns it
	// unchanged, which would make the blind gate below unreachable.
	if len(nonEmpty) >= 5 && rtkHasDuplication(lines) {
		return &rtkFilter{"dedup-log", rtkDedupLog}
	}
	if blind && len(strings.Split(text, "\n")) >= rtkTruncMinLines {
		return &rtkFilter{"smart-truncate", rtkSmartTruncate}
	}
	return nil
}

// rtkHasDuplication reports whether lines holds a run of repeated non-blank
// lines or two consecutive blank lines, the only cases dedupLog shrinks.
func rtkHasDuplication(lines []string) bool {
	prev, prevBlank := "", false
	for _, l := range lines {
		blank := strings.TrimSpace(l) == ""
		if blank && prevBlank {
			return true
		}
		if !blank && l == prev {
			return true
		}
		prev, prevBlank = l, blank
	}
	return false
}

func isGrepLine(line string) bool {
	f1 := strings.IndexByte(line, ':')
	if f1 < 0 {
		return false
	}
	rel := strings.IndexByte(line[f1+1:], ':')
	if rel < 0 {
		return false
	}
	return isAllDigits(line[f1+1 : f1+1+rel])
}

func rtkAllPathLike(lines []string) bool {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			return false
		}
		// windows absolute path; trailing colons tolerated
		if reWinDrive.MatchString(t) {
			continue
		}
		if strings.Contains(t, ":") {
			return false
		}
		if !strings.HasPrefix(t, ".") && !strings.HasPrefix(t, "/") && !strings.Contains(t, "/") {
			return false
		}
	}
	return true
}

func rtkMostlyPorcelain(lines []string) bool {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) < 3 {
		return false
	}
	hits := 0
	for _, l := range nonEmpty {
		if rePorcelain.MatchString(l) {
			hits++
		}
	}
	return float64(hits)/float64(len(nonEmpty)) >= 0.6
}

func rtkIsLineNumbered(lines []string) bool {
	if len(lines) > 100 {
		lines = lines[:100]
	}
	hits, nonEmpty := 0, 0
	for _, l := range lines {
		if len(l) == 0 {
			continue
		}
		nonEmpty++
		if reNumberedLine.MatchString(l) {
			hits++
		}
	}
	return nonEmpty >= 5 && float64(hits)/float64(nonEmpty) >= rtkReadNumMinRatio
}

func rtkCountMatches(text string, re *regexp.Regexp) int {
	return len(re.FindAllString(text, -1))
}

// --- driver ---

type rtkStats struct {
	bytesBefore int
	bytesAfter  int
	hits        []rtkHit
}

type rtkHit struct {
	filter string
	saved  int
}

func (s *rtkStats) saved() int { return s.bytesBefore - s.bytesAfter }

func (s *rtkStats) log() string {
	if s == nil || len(s.hits) == 0 {
		return ""
	}
	pct := 0.0
	if s.bytesBefore > 0 {
		pct = float64(s.saved()) / float64(s.bytesBefore) * 100
	}
	seen := map[string]bool{}
	var names []string
	for _, h := range s.hits {
		if !seen[h.filter] {
			seen[h.filter] = true
			names = append(names, h.filter)
		}
	}
	return fmt.Sprintf("[RTK] saved %dB / %dB (%.1f%%) via [%s] hits=%d",
		s.saved(), s.bytesBefore, pct, strings.Join(names, ","), len(s.hits))
}

// rtkCompressText picks a filter for text and applies it. returns the input
// unchanged when nothing applies, when the result would be empty, or when the
// result grew. blind gates the last-resort truncation of unidentifiable blobs.
func rtkCompressText(text string, st *rtkStats, blind bool) string {
	n := len(text)
	st.bytesBefore += n
	if n < rtkMinCompressSize || n > rtkRawCap {
		st.bytesAfter += n
		return text
	}
	fn := rtkAutoDetect(text, blind)
	if fn == nil {
		st.bytesAfter += n
		return text
	}
	out := fn.call(text)
	// safety: never empty, never larger than the input
	if out == "" || len(out) >= n {
		st.bytesAfter += n
		return text
	}
	st.bytesAfter += len(out)
	st.hits = append(st.hits, rtkHit{filter: fn.name, saved: n - len(out)})
	return out
}

// --- helpers ---

// rtkShrink returns out only when it is a real saving over the input.
func rtkShrink(in, out string) string {
	if out == "" || len(out) >= len(in) {
		return in
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

type extCount struct {
	k string
	n int
}

func sortedCounts(m map[string]int) []extCount {
	out := make([]extCount, 0, len(m))
	for k, n := range m {
		out = append(out, extCount{k, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n > out[j].n })
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func humanSize(b int) string {
	switch {
	case b >= 1<<20:
		return strconv.FormatFloat(float64(b)/(1<<20), 'f', 1, 64) + "M"
	case b >= 1024:
		return strconv.FormatFloat(float64(b)/1024, 'f', 1, 64) + "K"
	default:
		return strconv.Itoa(b) + "B"
	}
}

// parseLsLine pulls type/size/name out of an `ls -la` row.
func parseLsLine(line string) (kind byte, size int, name string, ok bool) {
	m := reLsDate.FindStringIndex(line)
	if m == nil {
		return 0, 0, "", false
	}
	name = line[m[1]:]
	before := strings.Fields(line[:m[0]])
	if len(before) < 4 {
		return 0, 0, "", false
	}
	kind = before[0][0]
	for i := len(before) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(before[i]); err == nil && strconv.Itoa(n) == before[i] {
			return kind, n, name, true
		}
	}
	return kind, 0, name, true
}
