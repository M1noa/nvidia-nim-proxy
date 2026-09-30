package main

// detection regexes for the RTK port. see rtk.go for the filters.

import "regexp"

var (
	reGitLog          = regexp.MustCompile(`(?m)^[*|/\\ ]*commit [0-9a-fA-F]{7,40}$`)
	reGitDiff         = regexp.MustCompile(`(?m)^diff --git `)
	reGitDiffHunk     = regexp.MustCompile(`(?m)^@@ `)
	reGitStatus       = regexp.MustCompile(`(?m)^On branch |^nothing to commit|^Changes (not |to be )|^Untracked files:`)
	rePorcelain       = regexp.MustCompile(`(?m)^[ MADRCU?!][ MADRCU?!] \S`)
	rePorcelainPrefix = regexp.MustCompile(`^[ MADRCU?!][ MADRCU?!] `)
	reOnBranch        = regexp.MustCompile(`^On branch (\S+)`)
	reLongStatus      = regexp.MustCompile(`^\s*(modified|new file|deleted|renamed|both modified):\s+(.+)$`)
	reUntrackedHeader = regexp.MustCompile(`(?i)^Untracked files:`)

	reBuildOut = regexp.MustCompile(`(?im)^(npm (warn|error|ERR!)|yarn (warn|error)|\s*Compiling\s+\S+|\s*Downloading\s+\S+|added \d+ package|\[ERROR\]|BUILD (SUCCESS|FAILED)|\s*Finished\s+|Successfully (installed|built)|ERROR:)`)

	reNpmErr        = regexp.MustCompile(`(?i)^npm (ERR!|error)`)
	reYarnErr       = regexp.MustCompile(`(?i)^yarn error`)
	reNpmDeprecated = regexp.MustCompile(`(?i)^npm warn deprecated`)
	reNpmWarn       = regexp.MustCompile(`(?i)^npm warn`)
	reYarnWarn      = regexp.MustCompile(`(?i)^yarn warn`)
	reErrLabel      = regexp.MustCompile(`(?i)^error(\[|:)|^error -->`)
	reWarnLabel     = regexp.MustCompile(`(?i)^warning(\[|:)|^warning -->`)
	reErrUpper      = regexp.MustCompile(`(?i)^ERROR:`)
	reBrackErr      = regexp.MustCompile(`(?i)^\[ERROR\]|^BUILD FAILED`)
	reBrackWarn     = regexp.MustCompile(`(?i)^\[WARNING\]`)
	reCompiling     = regexp.MustCompile(`(?i)^\s*Compiling\s+\S+`)
	reDownloading   = regexp.MustCompile(`(?i)^\s*Downloading\s+\S+|^Fetching\s+`)
	reBuildSummary  = regexp.MustCompile(`(?i)^(added|removed|changed|audited|installed)\s+\d+\s+package|^\s*Finished\s+|^BUILD SUCCESS|^\d+\s+(vulnerabilities|packages?|warnings?|errors?)|^Successfully (installed|built)|^To address .* issues|^Run ` + "`npm (audit|fund)`" + `|packages are looking for funding`)
	reCargoErrCont  = regexp.MustCompile(`^\s*(-->|\||\d+\s*\||=)`)

	reTreeGlyph    = regexp.MustCompile(`[├└]──|│  `)
	reLsRow        = regexp.MustCompile(`(?m)^[-dlbcps][rwx-]{9}`)
	reLsTotal      = regexp.MustCompile(`(?m)^total \d+$`)
	reLsDate       = regexp.MustCompile(`\s+(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\s+(\d{4}|\d{2}:\d{2})\s+`)
	reSearchHeader = regexp.MustCompile(`^Result of search in '[^']*' \(total (\d+) files?\):`)
	reNumberedLine = regexp.MustCompile(`^\s*\d+\|`)
	reWinDrive     = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

	reCommitHdr    = regexp.MustCompile(`(?i)^(commit [0-9a-f]{7,40}$|[*|/\\ ]+commit [0-9a-f]{7,40})`)
	reAuthorDate   = regexp.MustCompile(`(?i)^[*|/\\ ]*(Author|Date):`)
	reIndented     = regexp.MustCompile(`^[*|/\\ ]*    \S`)
	reFilesChanged = regexp.MustCompile(`^\d+ file\w* changed`)
	reGraphEntry   = regexp.MustCompile(`(?i)^[*|/\\ ]+[0-9a-f]{7,40}\s+.+`)
	reOneline      = regexp.MustCompile(`(?i)^[0-9a-f]{7,40}\s+`)
	reGraphOnly    = regexp.MustCompile(`^[*|/\\ ]+$`)
)
