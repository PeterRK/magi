package prompt

import (
	"embed"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strings"
)

//go:embed prompts/*.md
var promptFiles embed.FS

// Version identifies the embedded role instructions. Change it
// whenever those instructions or their model-visible tool guidance change.
// An explicitly requested rollback restores the baseline version.
const Version = "magi-native-v381"

func rolePrompt(purpose string) (string, error) {
	name := purpose
	if name == "review" {
		name = "reviewer"
	}
	switch name {
	case "planner", "reviewer", "worker", "interaction", "interaction_summary":
	default:
		return "", fmt.Errorf("unknown native prompt purpose %q", purpose)
	}
	specific, err := promptFiles.ReadFile("prompts/" + name + ".md")
	if err != nil {
		return "", err
	}
	return string(specific), nil
}

func ForPurpose(purpose string) (string, error) {
	specific, err := rolePrompt(purpose)
	if err != nil {
		return "", err
	}
	if purpose == "interaction" || purpose == "interaction_summary" {
		return specific, nil
	}
	common, err := promptFiles.ReadFile("prompts/common.md")
	if err != nil {
		return "", err
	}
	return string(common) + "\n" + specific, nil
}

func ForCompaction() (string, error) {
	instructions, err := promptFiles.ReadFile("prompts/compaction.md")
	return string(instructions), err
}

func ToolDescription(name string) string {
	switch name {
	case "read":
		return "Read UTF-8 text by 1-based lines, or list direct directory entries, only inside ProjectRoot (the workspace root containing .magi). Paths must be project-relative; absolute paths and parent traversal are rejected. .magi and .git are protected metadata: do not request their contents; access is rejected and directory listings omit them. path is required on every call, including continuation pages; offset defaults to 1; limit defaults to 2000 and cannot exceed 2000. Source output is bounded to 50 KiB before JSON encoding; line prefixes show line numbers. next_offset gives the next unread line/entry when available; lines_clipped marks lines clipped at 8 KiB. Links return link text without following them. Saved tool output paths in .magi-temp can be read directly; general directory listings omit this temporary directory."
	case "glob":
		return "Find files only inside ProjectRoot (the workspace root containing .magi), under optional project-relative path (default .). Absolute paths and parent traversal are rejected. .magi and .git are protected metadata: do not target them with paths or patterns; their contents are excluded. pattern matches paths relative to that directory: *, ?, character classes and ** as a complete component for any directory depth, e.g. **/*.go. Returns a files array with up to 100 project-relative paths and 50 KiB before JSON encoding; truncated marks incomplete results. Does not follow links or exclude ordinary hidden, dependency or Git-ignored files; narrow the path/pattern instead of listing the whole repository repeatedly."
	case "grep":
		return "Search UTF-8 text only inside ProjectRoot (the workspace root containing .magi), in a project-relative file or directory (path defaults to .). Absolute paths and parent traversal are rejected. .magi and .git are protected metadata: do not target them with paths or include patterns; their contents are not searched. pattern uses Go/RE2 regular expressions, without lookaround/backreferences; (?i) enables case-insensitive matching. Optional include is a glob file filter, using basename when it has no slash, otherwise relative to the selected directory. Returns matches grouped by project-relative path, with 1-based N: line prefixes; up to 100 matching lines total/50 KiB before JSON encoding. Lines are matched before display clipping; previews retain up to 2 KiB of source text around the first match (its beginning if wider), with [prefix omitted]/[suffix omitted] markers. Read the source file at the given line for complete text; no search dump is saved. truncated also marks clipped previews. Links are not searched. NUL or invalid UTF-8 stops that file with truncated; earlier text matches are retained. Lines over 8 KiB are skipped with explicit truncation. Narrow the scope for further results. To search saved tool output, set path to the returned output file; general searches skip .magi-temp."
	case "bash":
		return "Run the command string as a Linux Bash script (single or multiple lines) with --noprofile --norc -c in ProjectRoot or optional project-relative workdir. Do not access files outside this call's target working directory: the resolved workdir when supplied, otherwise ProjectRoot (the workspace root containing .magi). This includes reading, searching, writing and deleting, whether directly or through scripts, links or other commands. Keep temporary project files inside that directory too. Do not directly read, write, delete or otherwise manipulate .magi or .git metadata, including nested metadata directories; use git commands for Git operations. Magi enforces a per-command timeout: timeout is an optional positive integer number of seconds, default 120; set it explicitly for longer work. The caller deadline and cancellation still apply. On timeout, the process group is killed and joined, captured output is retained, and status is timed_out; inspect diagnostics before retrying. Keep subprocesses in this process group; do not detach them. Only PATH, HOME, LANG, LC_ALL, TERM and TMPDIR are inherited. stdout/stderr are saved in .magi-temp with tail previews in stdout_tail/stderr_tail, each limited to the last 2000 lines or 50 KiB, whichever comes first. Use read or grep on the returned saved output paths for earlier output, not another execution. Dump paths are omitted when the captured output fits entirely in the inline tail. Diagnostics report incomplete capture or saved files; capped files contain a prefix. These files are cleared when the next round starts; preserve needed evidence in your report or a project artifact."
	case "write":
		return "Create or overwrite one UTF-8 text file inside ProjectRoot using path and content. path must be project-relative and cannot traverse parents or enter .magi/.git metadata; links, special files and private initialization directories are rejected. Missing parent directories are created. content is written exactly, including its line endings and final newline; no newline or formatting is added. Existing file permissions are preserved. Maximum 16 MiB per file; NUL is rejected. Publication is atomic. Returns path and operation (created or updated). Use edit for a targeted replacement; use bash for moves or deletion."
	case "edit":
		return "Replace exact text in one existing UTF-8 file inside ProjectRoot using path, old_string and new_string. Every edit call requires its own path; never inherit a target from an earlier call. path must be project-relative and cannot traverse parents or enter .magi/.git metadata; links, special files and private initialization directories are rejected. old_string must be nonempty and differ from new_string. By default it must match exactly once: no match or multiple matches leaves the file unchanged; reread the relevant text or include more context. Set replace_all true only to replace every non-overlapping occurrence. LF and CRLF are equivalent for matching; the source newline style, UTF-8 BOM and file permissions are preserved. Mixed source line endings, NUL and files/results over 16 MiB are rejected. Publication is atomic. Returns path and replacements; no patch headers or diff syntax."

	case "history_read":
		return "Retrieve one archived body for a consequential detail missing from current messages. Its ref must already appear as a [[...]] citation in supplied task material or tool results in this context; do not generate a ref or predict it from numbering. Bare ID fields and instruction examples do not qualify. Before calling, match the target ref against supplied message ref fields and earlier successful history_read calls; reuse any body already present. The result is immutable plain text; citations inside it are not expanded."
	case "git_log":
		return "Read recent Git commits in ProjectRoot, without modifying files/index or fetching. By default includes local and remote-tracking branches plus HEAD, deduplicated; optional ref restricts history to one commit or branch. Supply only a known existing ref; omit ref to discover available branches and HEAD. limit defaults to 10 (0 also defaults), maximum 50. Returns content with at least 12-character commit IDs, references and subjects. content retains complete leading lines, up to 2000 lines or 50 KiB, whichever comes first. When longer, dump names saved output (up to 16 MiB by default) for read/grep until the next round; next_offset gives the first unread line in dump when available. truncated marks omitted commit entries or incomplete retention; diagnostic explains unavailable or capped files. Does not include unreachable commits from deleted branches."
	case "git_diff":
		return "Read committed Git changes in ProjectRoot. ref is required (short commit ID or branch). Without base, show that commit's changes against its first parent; a root commit shows added content. With base, show the net diff from base to ref. Optional paths are literal project-relative files/directories, not globs; parent traversal, absolute paths and .magi/.git metadata are rejected. Protected metadata and .magi-temp are excluded even without paths. Returns resolved commit IDs and content containing a patch, retaining complete leading lines, up to 2000 lines or 50 KiB, whichever comes first. When longer, dump names saved output (up to 16 MiB by default) for read/grep until the next round; next_offset gives the first unread line in dump when available. Prefer narrowing paths. truncated and diagnostic disclose incomplete retention or unavailable files. Use read for needed current-code context. Does not show uncommitted or untracked content; git_status locates those paths. External diff, textconv and submodule traversal are disabled."
	case "git_status":
		return "Read current Git branch and short status in ProjectRoot, including staged, unstaged and untracked paths. Takes no arguments. In content, X is index status and Y is working-tree status; ?? means untracked, ## is the branch header. Does not write the index, execute content filters, fetch or recurse into submodule working trees. Protected metadata and .magi-temp are excluded. content retains complete leading lines, up to 2000 lines or 50 KiB, whichever comes first. When longer, dump names saved output (up to 16 MiB by default) for read/grep until the next round; next_offset gives the first unread line in dump when available. truncated and diagnostic disclose incomplete retention or unavailable files."
	case "submit_plan":
		return "Submit this round's orders and memo, both required. orders is an array of instruction strings carrying required outcomes, scope and necessary constraints; use R refs for adopted findings and W refs for prior work as needed. Put your analysis in memo and leave implementation methods to Worker. orders may be []. memo records dispatch intent, work not yet assigned, risks and pending decisions/discussion; it is not a user-input summary. memo must be nonempty with orders. When no work or planning matter remains, submit empty orders and empty memo; present the final explanation in Interact. Orders and nonempty memo receive P refs; returns order_ids and ref for memo. Success ends this planning call and freezes both; user interaction runs separately. This call must be alone."
	case "submit_review":
		return "Submit this round's findings and necessary reminders in summary for Planner, with consequential evidence and unresolved doubts. Returns the report's ref; the full review context is retained. This call must be alone."
	case "finish_job":
		return "Submit the report and end this Worker job. Describe actual effects at the assigned uses, validation, unfinished duties and doubts. Distinguish observed results, implementation choices, inferred behavior and remaining uncertainty. Report submission does not certify task completion. This call must be alone."
	case "show_user":
		return "Present a natural-language message to the user, without internal history markers or execution IDs. Returns a display acknowledgement; user input arrives independently. Set wait_for_reply=true only for a concrete question requiring the user's answer; otherwise false, including progress updates, acknowledgements and completion notices. Use at least once per interaction, including before closure. This call must be alone."

	}
	return ""
}

// Runtime instructions share the embedded prompt pack. Core supplies the
// trigger and dynamic context; this package owns the model-facing wording.
var WorkerWrapUp = PhaseInstruction("WORKER_WRAP_UP", "Use only the existing context to summarize already settled progress, unfinished obligations and useful references, then call finish_job, the only available tool. Do not initiate implementation, tests, repository investigation, external commands or user questions. Describe unfinished work and useful partial progress accurately.")

// UserReplySources keeps current-interaction provenance available after Full.
func UserReplySources(refs []string) string {
	return "\nUser reply sources in received order: [[" + strings.Join(refs, "]], [[") + "]]."
}

// CompactionBoundaries identifies original messages retained after replacement.
func CompactionBoundaries(head, tail int) string {
	return fmt.Sprintf("\nThe first %d original messages remain as the fixed head after compaction. Avoid unnecessary repetition where practical; a little compatible overlap is acceptable.\nThe %d recent original messages are retained separately and are not included here.", head, tail)
}

const CompactionTask = "The selected history ends here. This request is only for compaction; do not resume historical workflows or phase instructions. Return only the checkpoint as plain text, without a JSON wrapper or a tool call."

func FullCapacity(ceiling, target int) string {
	return fmt.Sprintf(" Keep the complete replacement below %d tokens; aim for about %d tokens where feasible.", ceiling, target)
}

func FullCapacityRepair(input, ceiling int) string {
	return PhaseInstruction("CONTEXT_SUMMARY_REPAIR", fmt.Sprintf("Full replacement input is %d tokens and must remain below %d. Rewrite a shorter checkpoint preserving current duties and useful known references.", input, ceiling))
}

func ProtocolReminder(detail string) string {
	return PhaseInstruction("PROTOCOL_REPAIR", detail)
}

// PhaseInstruction encodes the content of a runtime system instruction.
// Callers own the trigger and message envelope; prompt owns the content format.
func PhaseInstruction(phase, instruction string) string {
	body, _ := jsonv2.Marshal(struct {
		Phase       string `json:"phase"`
		Instruction string `json:"instruction"`
	}{phase, instruction})
	return string(body)
}
