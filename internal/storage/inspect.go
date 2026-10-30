package storage

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"

	"magi/internal/core/protocol"
)

// Inspection contains public metadata, never role contexts or history bodies.
type Inspection struct {
	FormatVersion int               `json:"format_version"`
	ProjectID     string            `json:"project_id"`
	ProjectRoot   string            `json:"project_root"`
	Counters      Counters          `json:"counters"`
	Usage         UsageReport       `json:"usage"`
	Rounds        []RoundInspection `json:"rounds"`
}

type RoundInspection struct {
	Metadata       protocol.StoredRound `json:"metadata"`
	SnapshotPath   string               `json:"snapshot_path"`
	SnapshotStatus string               `json:"snapshot_status"`
	Retained       bool                 `json:"retained"`
}

// UsageTotals uses exact integers even when sums exceed a single stored counter.
type UsageTotals struct {
	InputTokens     *big.Int `json:"input_tokens"`
	OutputTokens    *big.Int `json:"output_tokens"`
	TotalTokens     *big.Int `json:"total_tokens"`
	CacheHitTokens  *big.Int `json:"cache_hit_tokens"`
	CacheMissTokens *big.Int `json:"cache_miss_tokens"`
	CacheHitRate    *float64 `json:"cache_hit_rate"`
}

// UsageReport contains batched role usage, not billing totals. Unflushed usage
// may be lost on interruption; compaction is counted toward its owner role.
type UsageReport struct {
	Planner  UsageTotals `json:"planner"`
	Worker   UsageTotals `json:"worker"`
	Reviewer UsageTotals `json:"reviewer"`
	Total    UsageTotals `json:"total"`
}

func usageReport(g Counters) UsageReport {
	var report UsageReport
	var hit, miss, output big.Int
	for _, entry := range []struct {
		usage  protocol.TokenUsage
		target *UsageTotals
	}{{g.Planner, &report.Planner}, {g.Worker, &report.Worker}, {g.Reviewer, &report.Reviewer}} {
		u := entry.usage
		*entry.target = summarizeUsage(new(big.Int).SetUint64(u.Hit), new(big.Int).SetUint64(u.Miss), new(big.Int).SetUint64(u.Output))
		hit.Add(&hit, entry.target.CacheHitTokens)
		miss.Add(&miss, entry.target.CacheMissTokens)
		output.Add(&output, entry.target.OutputTokens)
	}
	report.Total = summarizeUsage(&hit, &miss, &output)
	return report
}

func summarizeUsage(hit, miss, output *big.Int) UsageTotals {
	input := new(big.Int).Add(hit, miss)
	u := UsageTotals{InputTokens: input, OutputTokens: output, TotalTokens: new(big.Int).Add(input, output), CacheHitTokens: hit, CacheMissTokens: miss}
	if input.Sign() != 0 {
		rate, _ := new(big.Rat).SetFrac(hit, input).Float64()
		u.CacheHitRate = &rate
	}
	return u
}

// Inspect opens only existing state in read-only mode. It neither initializes,
// quarantines, flushes usage nor binds the active workspace's global resources.
// Missing expired snapshots are expected; damaged retained snapshots are shown.
func Inspect(ctx context.Context, root string, usageOnly bool) (report Inspection, returnErr error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return report, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return report, err
	}
	dir := filepath.Join(root, ".magi")
	info, err := os.Lstat(dir)
	if err != nil {
		return report, err
	}
	if !info.IsDir() {
		return report, protocol.IntegrityFailure("state path is not a physical directory")
	}
	metadata, err := openMetadata(filepath.Join(dir, "project.json"), false)
	if err != nil {
		return report, err
	}
	s, err := open(ctx, dir, false, true)
	if err != nil {
		return report, err
	}
	defer func() { returnErr = errors.Join(returnErr, s.db.Close()) }()
	g, err := s.Counters(ctx)
	if err != nil {
		return report, err
	}
	report = Inspection{FormatVersion: metadata.FormatVersion, ProjectRoot: root, ProjectID: metadata.ProjectID, Counters: g, Usage: usageReport(g), Rounds: []RoundInspection{}}
	if usageOnly {
		return report, nil
	}
	rounds, err := s.rounds(ctx, 0)
	if err != nil {
		return report, err
	}
	for i, round := range rounds {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		r := RoundInspection{Metadata: round, Retained: i < retainedRoundCount, SnapshotPath: filepath.Join(".magi", "safe-points", round.Hash)}
		if err := s.validate(filepath.Join("safe-points", round.Hash)); err != nil {
			return report, err
		}
		body, err := os.ReadFile(filepath.Join(root, r.SnapshotPath))
		switch {
		case os.IsNotExist(err) && !r.Retained:
			r.SnapshotStatus = "pruned"
		case os.IsNotExist(err):
			r.SnapshotStatus = "missing"
		case err != nil:
			r.SnapshotStatus = "unreadable"
		case hashBytes(body) != round.Hash:
			r.SnapshotStatus = "corrupt"
		default:
			r.SnapshotStatus = "valid"
		}
		report.Rounds = append(report.Rounds, r)
	}
	return report, nil
}
