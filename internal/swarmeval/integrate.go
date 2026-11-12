package swarmeval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Integrate 将同基线的无冲突文件改动组合成新的独立候选，不修改源工作区。
// 同一文件的不同内容及文件/目录冲突保守拒绝，留给显式集成修复处理。
func (r *Runner) Integrate(ctx context.Context, operationID string, inputs []*Artifact) (*Artifact, error) {
	if len(inputs) < 1 || len(inputs) > 16 {
		return nil, errors.New("集成需要 1–16 个候选")
	}
	dest, err := r.location("artifacts", operationID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	inputIDs := []string{}
	allIDs := map[string]bool{}
	var base []fileData
	files := map[string]fileData{}
	changed := map[string]*fileData{}
	var baseHash, baseRevision string
	for _, input := range inputs {
		if input == nil {
			return nil, errors.New("缺少集成候选")
		}
		a, err := r.LoadArtifact(ctx, input.ID)
		if err != nil {
			return nil, err
		}
		if input.ContentHash != a.ContentHash {
			return nil, ErrConflict
		}
		if seen[a.ID] {
			return nil, fmt.Errorf("%w：重复集成候选", ErrConflict)
		}
		seen[a.ID] = true
		lineage := append([]string{a.ID}, a.Inputs...)
		for _, id := range lineage {
			if allIDs[id] {
				return nil, fmt.Errorf("%w：候选已包含相同输入", ErrConflict)
			}
			allIDs[id] = true
		}
		if err := r.sourceMatches(ctx, a); err != nil {
			return nil, err
		}
		if base == nil {
			base, err = r.readTree(ctx, filepath.Join(filepath.Dir(a.Directory), "base"), a.BaseFiles)
			if err != nil {
				return nil, err
			}
			baseHash = a.BaseHash
			baseRevision = a.BaseRevision
			for _, f := range base {
				files[f.Path] = f
			}
		} else if a.BaseHash != baseHash || a.BaseRevision != baseRevision {
			return nil, fmt.Errorf("%w：集成基线不一致", ErrConflict)
		}
		current, err := r.readTree(ctx, a.Directory, a.Files)
		if err != nil {
			return nil, err
		}
		byPath := map[string]fileData{}
		for _, f := range current {
			byPath[f.Path] = f
		}
		for _, name := range a.ChangedPaths {
			var next *fileData
			if f, ok := byPath[name]; ok {
				copy := f
				next = &copy
			}
			for previous, prior := range changed {
				if previous == name {
					if !sameFile(prior, next) {
						return nil, fmt.Errorf("%w：文件 %s 的候选冲突", ErrConflict, name)
					}
				} else if strings.HasPrefix(previous, name+"/") || strings.HasPrefix(name, previous+"/") {
					return nil, fmt.Errorf("%w：文件和目录冲突", ErrConflict)
				}
			}
			changed[name] = next
			if next == nil {
				delete(files, name)
			} else {
				files[name] = *next
			}
		}
		inputIDs = append(inputIDs, a.ID)
	}
	merged := make([]fileData, 0, len(files))
	var total int64
	for _, f := range files {
		merged = append(merged, f)
		total += f.Size
	}
	if len(merged) > r.Limits.normalized().MaxFiles || total > r.Limits.normalized().MaxTotalBytes {
		return nil, errors.New("集成候选超过上限")
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Path < merged[j].Path })
	a := &Artifact{Version: 1, ID: operationID, Directory: filepath.Join(dest, "tree"), HeadRevision: baseRevision, BaseRevision: baseRevision, BaseHash: baseHash,
		ContentHash: treeHash(merged), Files: manifest(merged), BaseFiles: manifest(base), ChangedPaths: changedPaths(base, merged), Inputs: inputIDs, Isolation: IsolationLocalSnapshot, CreatedAt: time.Now().UTC()}
	if old, loadErr := r.LoadArtifact(ctx, operationID); loadErr == nil {
		if old.ContentHash == a.ContentHash && old.BaseHash == a.BaseHash && equalStrings(old.Inputs, a.Inputs) {
			return old, nil
		}
		return nil, ErrConflict
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return nil, loadErr
	}
	if err := r.writeArtifact(dest, a, merged, base); err != nil {
		return nil, err
	}
	return a, nil
}

func sameFile(a, b *fileData) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.File == b.File
}

// Detach 固定已有候选，与其仍在变动的源目录解除关联。结果仅证明冻结内容，
// 不证明原工作区的当前状态；原候选身份保留在 Inputs 中。
func (r *Runner) Detach(ctx context.Context, operationID string, artifact *Artifact) (*Artifact, error) {
	if artifact == nil {
		return nil, errors.New("缺少冻结候选")
	}
	a, err := r.LoadArtifact(ctx, artifact.ID)
	if err != nil {
		return nil, err
	}
	if artifact.ContentHash != a.ContentHash {
		return nil, ErrConflict
	}
	dest, err := r.location("artifacts", operationID)
	if err != nil {
		return nil, err
	}
	files, err := r.readTree(ctx, a.Directory, a.Files)
	if err != nil {
		return nil, err
	}
	base, err := r.readTree(ctx, filepath.Join(filepath.Dir(a.Directory), "base"), a.BaseFiles)
	if err != nil {
		return nil, err
	}
	copy := *a
	copy.ID = operationID
	copy.Directory = filepath.Join(dest, "tree")
	copy.SourceDir = ""
	copy.Inputs = append([]string{a.ID}, a.Inputs...)
	copy.CreatedAt = time.Now().UTC()
	if old, loadErr := r.LoadArtifact(ctx, operationID); loadErr == nil {
		if old.ContentHash == copy.ContentHash && old.BaseHash == copy.BaseHash && equalStrings(old.Inputs, copy.Inputs) && old.SourceDir == "" {
			return old, nil
		}
		return nil, ErrConflict
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return nil, loadErr
	}
	if err := r.writeArtifact(dest, &copy, files, base); err != nil {
		return nil, err
	}
	return &copy, nil
}
