// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snapclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/br/pkg/restore"
	"github.com/pingcap/tidb/br/pkg/restore/split"
	restoreutils "github.com/pingcap/tidb/br/pkg/restore/utils"
	"github.com/pingcap/tidb/br/pkg/summary"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"go.uber.org/zap"
)

func getSortedPhysicalTables(createdTables []*CreatedTable) []*PhysicalTable {
	physicalTables := make([]*PhysicalTable, 0, len(createdTables))
	for _, createdTable := range createdTables {
		physicalTables = append(physicalTables, &PhysicalTable{
			NewPhysicalID: createdTable.Table.ID,
			OldPhysicalID: createdTable.OldTable.Info.ID,
			RewriteRules:  createdTable.RewriteRule,
		})

		partitionIDMap := restoreutils.GetPartitionIDMap(createdTable.Table, createdTable.OldTable.Info)
		for oldID, newID := range partitionIDMap {
			physicalTables = append(physicalTables, &PhysicalTable{
				NewPhysicalID: newID,
				OldPhysicalID: oldID,
				RewriteRules:  createdTable.RewriteRule,
			})
		}
	}
	// sort the physical table by downstream stream physical id
	sort.Slice(physicalTables, func(a, b int) bool {
		return physicalTables[a].NewPhysicalID < physicalTables[b].NewPhysicalID
	})
	return physicalTables
}

// mapTableToFiles makes a map that mapping table ID to its backup files.
// aware that one file can and only can hold one table.
func mapTableToFiles(files []*backuppb.File) (map[int64][]*backuppb.File, int) {
	result := map[int64][]*backuppb.File{}
	// count the write cf file that hint for split key slice size
	maxSplitKeyCount := 0
	for _, file := range files {
		tableID := tablecodec.DecodeTableID(file.GetStartKey())
		tableEndID := tablecodec.DecodeTableID(file.GetEndKey())
		if tableID != tableEndID {
			log.Panic("key range spread between many files.",
				zap.String("file name", file.Name),
				logutil.Key("startKey", file.StartKey),
				logutil.Key("endKey", file.EndKey))
		}
		if tableID == 0 {
			log.Panic("invalid table key of file",
				zap.String("file name", file.Name),
				logutil.Key("startKey", file.StartKey),
				logutil.Key("endKey", file.EndKey))
		}
		result[tableID] = append(result[tableID], file)
		if file.Cf == restoreutils.WriteCFName {
			maxSplitKeyCount += 1
		}
	}
	return result, maxSplitKeyCount
}

// filterOutFiles filters out files that exist in the checkpoint set.
func filterOutFiles(checkpointSet map[string]struct{}, files []*backuppb.File, onProgress func(int64)) []*backuppb.File {
	progress := int(0)
	totalKVs := uint64(0)
	totalBytes := uint64(0)
	newFiles := make([]*backuppb.File, 0, len(files))
	for _, file := range files {
		rangeKey := getFileRangeKey(file.Name)
		if _, exists := checkpointSet[rangeKey]; exists {
			// the range has been import done, so skip it and
			// update the summary information
			progress += 1
			totalKVs += file.TotalKvs
			totalBytes += file.TotalBytes
		} else {
			newFiles = append(newFiles, file)
		}
	}
	if progress > 0 {
		// (split/scatter + download/ingest) / (default cf + write cf)
		onProgress(int64(progress) * 2 / 2)
		summary.CollectSuccessUnit(summary.TotalKV, progress, totalKVs)
		summary.CollectSuccessUnit(summary.SkippedKVCountByCheckpoint, progress, totalKVs)
		summary.CollectSuccessUnit(summary.TotalBytes, progress, totalBytes)
		summary.CollectSuccessUnit(summary.SkippedBytesByCheckpoint, progress, totalBytes)
	}
	return newFiles
}

// If there are many tables with only a few rows, the number of merged SSTs will be too large.
// So set a threshold to avoid it.
const MergedRangeCountThreshold = 1536

// SortAndValidateFileRanges sort, merge and validate files by tables and yields tables with range.
func SortAndValidateFileRanges(
	createdTables []*CreatedTable,
	allFiles []*backuppb.File,
	checkpointSetWithTableID map[int64]map[string]struct{},
	splitSizeBytes, splitKeyCount uint64,
	splitOnTable bool,
	onProgress func(int64),
) ([][]byte, []restore.BatchBackupFileSet, error) {
	sortedPhysicalTables := getSortedPhysicalTables(createdTables)
	// mapping table ID to its backup files
	fileOfTable, hintSplitKeyCount := mapTableToFiles(allFiles)
	// sort, merge, and validate files in each tables, and generate split keys by the way
	var (
		// to generate region split keys, merge the small ranges over the adjacent tables
		sortedSplitKeys        = make([][]byte, 0, hintSplitKeyCount)
		groupSize              = uint64(0)
		groupCount             = uint64(0)
		lastKey         []byte = nil

		// group the files by the generated split keys
		tableIDWithFilesGroup                            = make([]restore.BatchBackupFileSet, 0, hintSplitKeyCount)
		lastFilesGroup        restore.BatchBackupFileSet = nil

		// statistic
		mergedRangeCount = 0
	)

	log.Info("start to merge ranges", zap.Uint64("kv size threshold", splitSizeBytes), zap.Uint64("kv count threshold", splitKeyCount))
	for _, table := range sortedPhysicalTables {
		files := fileOfTable[table.OldPhysicalID]
		for _, file := range files {
			if err := restoreutils.ValidateFileRewriteRule(file, table.RewriteRules); err != nil {
				return nil, nil, errors.Trace(err)
			}
		}
		// Merge small ranges to reduce split and scatter regions.
		// Notice that the files having the same start key and end key are in the same range.
		sortedRanges, stat, err := restoreutils.MergeAndRewriteFileRanges(
			files, table.RewriteRules, splitSizeBytes, splitKeyCount)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		log.Info("merge and validate file",
			zap.Int64("new physical ID", table.NewPhysicalID),
			zap.Int64("old physical ID", table.OldPhysicalID),
			zap.Int("Files(total)", stat.TotalFiles),
			zap.Int("File(write)", stat.TotalWriteCFFile),
			zap.Int("File(default)", stat.TotalDefaultCFFile),
			zap.Int("Region(total)", stat.TotalRegions),
			zap.Int("Regoin(keys avg)", stat.RegionKeysAvg),
			zap.Int("Region(bytes avg)", stat.RegionBytesAvg),
			zap.Int("Merged(regions)", stat.MergedRegions),
			zap.Int("Merged(keys avg)", stat.MergedRegionKeysAvg),
			zap.Int("Merged(bytes avg)", stat.MergedRegionBytesAvg))

		// skip some ranges if recorded by checkpoint
		// Notice that skip ranges after select split keys in order to make the split keys
		// always the same.
		checkpointSet := checkpointSetWithTableID[table.NewPhysicalID]

		// Generate the split keys, and notice that the way to generate split keys must be deterministic
		// and regardless of the current cluster region distribution. Therefore, when restore fails, the
		// generated split keys keep the same as before the next time we retry to restore.
		//
		// Here suppose that all the ranges is in the one region at beginning.
		// In general, the ids of tables, which are created in the previous stage, are continuously because:
		//
		// 1. Before create tables, the cluster global id is allocated to ${GLOBAL_ID};
		// 2. Suppose the ids of tables to be created are {t_i}, which t_i < t_j if i < j.
		// 3. BR preallocate the global id from ${GLOBAL_ID} to t_max, so the table ids, which are larger
		//  than ${GLOBAL_ID}, has the same downstream ids.
		// 4. Then BR creates tables, and the table ids, which are less than or equal to ${GLOBAL_ID}, are
		//  allocated to [t_max + 1, ...) in the downstream cluster.
		// 5. Therefore, the BR-created tables are usually continuously.
		//
		// Besides, the prefix of the existing region's start key and end key should not be `t{restored_table_id}`.
		for _, rg := range sortedRanges {
			// split key generation
			afterMergedGroupSize := groupSize + rg.Size
			afterMergedGroupCount := groupCount + rg.Count
			if afterMergedGroupSize > splitSizeBytes || afterMergedGroupCount > splitKeyCount || mergedRangeCount > MergedRangeCountThreshold {
				log.Info("merge ranges across tables due to kv size/count or merged count threshold exceeded",
					zap.Uint64("merged kv size", groupSize),
					zap.Uint64("merged kv count", groupCount),
					zap.Int("merged range count", mergedRangeCount))
				groupSize, groupCount = rg.Size, rg.Count
				mergedRangeCount = 0
				// can not merge files anymore, so generate a new split key
				if lastKey != nil {
					sortedSplitKeys = append(sortedSplitKeys, lastKey)
				}
				// then generate a new files group
				if lastFilesGroup != nil {
					tableIDWithFilesGroup = append(tableIDWithFilesGroup, lastFilesGroup)
					// reset the lastFiltesGroup immediately because it is not always updated in each loop cycle.
					lastFilesGroup = nil
				}
			} else {
				groupSize, groupCount = afterMergedGroupSize, afterMergedGroupCount
			}
			// override the previous key, which may not become a split key.
			lastKey = rg.EndKey
			// mergedRangeCount increment by the number of files before filtered by checkpoint in order to make split keys
			// always the same as that from before execution.
			mergedRangeCount += len(rg.Files)
			// checkpoint filter out the import done files in the previous restore executions.
			// Notice that skip ranges after select split keys in order to make the split keys
			// always the same.
			newFiles := filterOutFiles(checkpointSet, rg.Files, onProgress)
			// append the new files into the group
			if len(newFiles) > 0 {
				if len(lastFilesGroup) == 0 || lastFilesGroup[len(lastFilesGroup)-1].TableID != table.NewPhysicalID {
					lastFilesGroup = append(lastFilesGroup, restore.BackupFileSet{
						TableID:      table.NewPhysicalID,
						SSTFiles:     nil,
						RewriteRules: table.RewriteRules,
					})
				}
				lastFilesGroup[len(lastFilesGroup)-1].SSTFiles = append(lastFilesGroup[len(lastFilesGroup)-1].SSTFiles, newFiles...)
			}
		}

		// If the config split-table/split-region-on-table is on, it skip merging ranges over tables.
		if splitOnTable {
			log.Info("merge ranges across tables due to split on table",
				zap.Uint64("merged kv size", groupSize),
				zap.Uint64("merged kv count", groupCount),
				zap.Int("merged range count", mergedRangeCount))
			groupSize, groupCount = 0, 0
			mergedRangeCount = 0
			// Besides, ignore the table's last key that might be chosen as a split key, because there
			// is already a table split key.
			lastKey = nil
			if lastFilesGroup != nil {
				tableIDWithFilesGroup = append(tableIDWithFilesGroup, lastFilesGroup)
				lastFilesGroup = nil
			}
		}
	}
	// append the key of the last range anyway
	if lastKey != nil {
		sortedSplitKeys = append(sortedSplitKeys, lastKey)
	}
	// append the last files group anyway
	if lastFilesGroup != nil {
		log.Info("merge ranges across tables due to the last group",
			zap.Uint64("merged kv size", groupSize),
			zap.Uint64("merged kv count", groupCount),
			zap.Int("merged range count", mergedRangeCount))
		tableIDWithFilesGroup = append(tableIDWithFilesGroup, lastFilesGroup)
	}
	return sortedSplitKeys, tableIDWithFilesGroup, nil
}

func (rc *SnapClient) RestoreTables(
	ctx context.Context,
	placementRuleManager PlacementRuleManager,
	createdTables []*CreatedTable,
	allFiles []*backuppb.File,
	checkpointSetWithTableID map[int64]map[string]struct{},
	splitSizeBytes, splitKeyCount uint64,
	splitOnTable bool,
	onProgress func(int64),
) error {
	if err := placementRuleManager.SetPlacementRule(ctx, createdTables); err != nil {
		return errors.Trace(err)
	}
	defer func() {
		err := placementRuleManager.ResetPlacementRules(ctx)
		if err != nil {
			log.Warn("failed to reset placement rules", zap.Error(err))
		}
	}()

	start := time.Now()
	sortedSplitKeys, tableIDWithFilesGroup, err := SortAndValidateFileRanges(createdTables, allFiles, checkpointSetWithTableID, splitSizeBytes, splitKeyCount, splitOnTable, onProgress)
	if err != nil {
		return errors.Trace(err)
	}
	log.Info("Restore Stage Duration", zap.String("stage", "merge ranges"), zap.Duration("take", time.Since(start)))

	newProgress := func(i int64) { onProgress(i) }
	start = time.Now()
	if err = rc.SplitPoints(ctx, sortedSplitKeys, newProgress, false); err != nil {
		return errors.Trace(err)
	}
	log.Info("Restore Stage Duration", zap.String("stage", "split regions"), zap.Duration("take", time.Since(start)))

	start = time.Now()
	if err = rc.RestoreSSTFiles(ctx, tableIDWithFilesGroup, newProgress); err != nil {
		return errors.Trace(err)
	}
	elapsed := time.Since(start)
	log.Info("Restore Stage Duration", zap.String("stage", "restore files"), zap.Duration("take", elapsed))

	summary.CollectSuccessUnit("files", len(allFiles), elapsed)
	return nil
}

// SplitRanges implements TiKVRestorer. It splits region by
// data range after rewrite.
func (rc *SnapClient) SplitPoints(
	ctx context.Context,
	sortedSplitKeys [][]byte,
	onProgress func(int64),
	isRawKv bool,
) error {
	splitClientOpts := make([]split.ClientOptionalParameter, 0, 2)
	splitClientOpts = append(splitClientOpts, split.WithOnSplit(func(keys [][]byte) {
		onProgress(int64(len(keys)))
	}))
	// TODO seems duplicate with metaClient.
	if isRawKv {
		splitClientOpts = append(splitClientOpts, split.WithRawKV())
	}

	splitter := split.NewRegionSplitter(split.NewClient(
		rc.pdClient,
		rc.pdHTTPClient,
		rc.tlsConf,
		maxSplitKeysOnce,
		rc.storeCount+1,
		splitClientOpts...,
	))

	return splitter.ExecuteSortedKeys(ctx, sortedSplitKeys)
}

func getFileRangeKey(f string) string {
	// the backup date file pattern is `{store_id}_{region_id}_{epoch_version}_{key}_{ts}_{cf}.sst`
	// so we need to compare with out the `_{cf}.sst` suffix
	idx := strings.LastIndex(f, "_")
	if idx < 0 {
		panic(fmt.Sprintf("invalid backup data file name: '%s'", f))
	}

	return f[:idx]
}

// RestoreSSTFiles tries to do something prepare work, such as set speed limit, and restore the files.
func (rc *SnapClient) RestoreSSTFiles(
	ctx context.Context,
	tableIDWithFilesGroup []restore.BatchBackupFileSet,
	onProgress func(int64),
) (retErr error) {
	failpoint.Inject("corrupt-files", func(v failpoint.Value) {
		if cmd, ok := v.(string); ok {
			switch cmd {
			case "corrupt-last-table-files": // skip some files and eventually return an error to make the restore fail
				tableIDWithFilesGroup = tableIDWithFilesGroup[:len(tableIDWithFilesGroup)-1]
				defer func() { retErr = errors.Errorf("skip the last table files") }()
			case "only-last-table-files": // check whether all the files, except last table files, are skipped by checkpoint
				for _, tableIDWithFiless := range tableIDWithFilesGroup[:len(tableIDWithFilesGroup)-1] {
					for _, tableIDWithFiles := range tableIDWithFiless {
						if len(tableIDWithFiles.SSTFiles) > 0 {
							log.Panic("has files but not the last table files")
						}
					}
				}
			}
		}
	})

	r := rc.GetRestorer(rc.checkpointRunner)
	retErr = r.GoRestore(onProgress, tableIDWithFilesGroup...)
	if retErr != nil {
		return retErr
	}
	return r.WaitUntilFinish()
}