package filesystemhelper

import (
	"fmt"
	"github.com/Altinity/clickhouse-backup/v2/pkg/utils"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Altinity/clickhouse-backup/v2/pkg/clickhouse"
	"github.com/Altinity/clickhouse-backup/v2/pkg/common"
	"github.com/Altinity/clickhouse-backup/v2/pkg/metadata"
	apexLog "github.com/apex/log"
)

var (
	uid       *int
	gid       *int
	chownLock sync.Mutex
)

// Chown - set permission on path to clickhouse user
// This is necessary that the ClickHouse will be able to read parts files on restore
func Chown(path string, ch *clickhouse.ClickHouse, disks []clickhouse.Disk, recursive bool) error {
	var (
		dataPath string
		err      error
	)
	if os.Getuid() != 0 {
		return nil
	}
	chownLock.Lock()
	if uid == nil {
		if dataPath, err = ch.GetDefaultPath(disks); err != nil {
			return err
		}
		info, err := os.Stat(dataPath)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		intUid := int(stat.Uid)
		intGid := int(stat.Gid)
		uid = &intUid
		gid = &intGid
	}
	chownLock.Unlock()
	if !recursive {
		return os.Chown(path, *uid, *gid)
	}
	return filepath.Walk(path, func(fName string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(fName, *uid, *gid)
	})
}

func Mkdir(name string, ch *clickhouse.ClickHouse, disks []clickhouse.Disk) error {
	if err := os.MkdirAll(name, 0750); err != nil && !os.IsExist(err) {
		return err
	}
	if err := Chown(name, ch, disks, false); err != nil {
		return err
	}
	return nil
}

func MkdirAll(path string, ch *clickhouse.ClickHouse, disks []clickhouse.Disk) error {
	// Fast path: if we can tell whether path is a directory or file, stop with success or error.
	dir, err := os.Stat(path)
	if err == nil {
		if dir.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}

	// Slow path: make sure parent exists and then call Mkdir for path.
	i := len(path)
	for i > 0 && os.IsPathSeparator(path[i-1]) { // Skip trailing path separator.
		i--
	}

	j := i
	for j > 0 && !os.IsPathSeparator(path[j-1]) { // Scan backward over element.
		j--
	}

	if j > 1 {
		// Create parent.
		err = MkdirAll(path[:j-1], ch, disks)
		if err != nil {
			return err
		}
	}

	// Parent now exists; invoke Mkdir and use its result.
	err = Mkdir(path, ch, disks)
	if err != nil {
		// Handle arguments like "foo/." by
		// double-checking that directory doesn't exist.
		dir, err1 := os.Lstat(path)
		if err1 == nil && dir.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// HardlinkBackupPartsToStorage - copy partitions for specific table to detached folder
func HardlinkBackupPartsToStorage(backupName string, backupTable metadata.TableMetadata, disks []clickhouse.Disk, diskMap map[string]string, tableDataPaths []string, ch *clickhouse.ClickHouse, toDetached bool) error {
	log := apexLog.WithFields(apexLog.Fields{"operation": "HardlinkBackupPartsToStorage"})
	start := time.Now()
	dstDataPaths := clickhouse.GetDisksByPaths(disks, tableDataPaths)
	dbAndTableDir := path.Join(common.TablePathEncode(backupTable.Database), common.TablePathEncode(backupTable.Table))
	if !toDetached {
		for backupDiskName := range backupTable.Parts {
			dstParentDir, dstParentDirExists := dstDataPaths[backupDiskName]
			if dstParentDirExists {
				// avoid to restore to non-empty to avoid attach in already dropped partitions, corner case
				existsFiles, err := os.ReadDir(dstParentDir)
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				for _, f := range existsFiles {
					if f.Name() != "detached" && !strings.HasSuffix(f.Name(), ".txt") {
						return fmt.Errorf("%s contains exists data %v, we can't restore directly via ATTACH TABLE, use `clickhouse->restore_as_attach=false` in your config", dstParentDir, existsFiles)
					}
				}
			}
		}
	}
	for backupDiskName := range backupTable.Parts {
		for _, part := range backupTable.Parts[backupDiskName] {
			dstParentDir, dstParentDirExists := dstDataPaths[backupDiskName]
			if !dstParentDirExists && part.RebalancedDisk == "" {
				return fmt.Errorf("dstDataPaths=%#v, not contains %s", dstDataPaths, backupDiskName)
			}
			if !dstParentDirExists && part.RebalancedDisk != "" {
				backupDiskName = part.RebalancedDisk
				dstParentDir, dstParentDirExists = dstDataPaths[part.RebalancedDisk]
				if !dstParentDirExists {
					return fmt.Errorf("dstDataPaths=%#v, not contains %s", dstDataPaths, part.RebalancedDisk)
				}
			}
			backupDiskPath := diskMap[backupDiskName]
			if toDetached {
				dstParentDir = filepath.Join(dstParentDir, "detached")

			}
			dstPartPath := filepath.Join(dstParentDir, part.Name)
			info, err := os.Stat(dstPartPath)
			if err != nil {
				if os.IsNotExist(err) {
					log.Debugf("MkDirAll %s", dstPartPath)
					if mkdirErr := MkdirAll(dstPartPath, ch, disks); mkdirErr != nil {
						log.Warnf("error during Mkdir %+v", mkdirErr)
					}
				} else {
					return err
				}
			} else if !info.IsDir() {
				return fmt.Errorf("'%s' should be directory or absent", dstPartPath)
			}
			srcPartPath := path.Join(backupDiskPath, "backup", backupName, "shadow", dbAndTableDir, backupDiskName, part.Name)
			if err := filepath.Walk(srcPartPath, func(filePath string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				// fix https://github.com/Altinity/clickhouse-backup/issues/826
				if strings.Contains(info.Name(), "frozen_metadata") {
					return nil
				}
				filename := strings.Trim(strings.TrimPrefix(filePath, srcPartPath), "/")
				dstFilePath := filepath.Join(dstPartPath, filename)
				if info.IsDir() {
					log.Debugf("MkDir %s", dstFilePath)
					return Mkdir(dstFilePath, ch, disks)
				}
				if !info.Mode().IsRegular() {
					log.Debugf("'%s' is not a regular file, skipping.", filePath)
					return nil
				}
				log.Debugf("Link %s -> %s", filePath, dstFilePath)
				if err := os.Link(filePath, dstFilePath); err != nil {
					if !os.IsExist(err) {
						return fmt.Errorf("failed to create hard link '%s' -> '%s': %w", filePath, dstFilePath, err)
					}
				}
				return Chown(dstFilePath, ch, disks, false)
			}); err != nil {
				return fmt.Errorf("error during filepath.Walk for part '%s': %w", part.Name, err)
			}
		}
	}
	log.WithField("duration", utils.HumanizeDuration(time.Since(start))).Debugf("done")
	return nil
}

func IsPartInPartition(partName string, partitionsBackupMap common.EmptyMap) bool {
	_, ok := partitionsBackupMap[strings.Split(partName, "_")[0]]
	return ok
}

func IsFileInPartition(disk, fileName string, partitionsBackupMap common.EmptyMap) bool {
	fileName = strings.TrimPrefix(fileName, disk+"_")
	fileName = strings.Split(fileName, "_")[0]
	if strings.Contains(fileName, "%") {
		decodedFileName, err := url.QueryUnescape(fileName)
		if err != nil {
			apexLog.Warnf("error decoding %s: %v", fileName, err)
			apexLog.Debugf("%s not found in %s, file will filtered", fileName, partitionsBackupMap)
			return false
		}
		fileName = decodedFileName
	}
	_, ok := partitionsBackupMap[fileName]
	return ok
}

func MoveShadowToBackup(shadowPath, backupPartsPath string, partitionsBackupMap common.EmptyMap, tableDiffFromRemote metadata.TableMetadata, disk clickhouse.Disk, version int) ([]metadata.Part, int64, error) {
	log := apexLog.WithField("logger", "MoveShadowToBackup")
	size := int64(0)
	parts := make([]metadata.Part, 0)
	err := filepath.Walk(shadowPath, func(filePath string, info os.FileInfo, err error) error {
		// fix https://github.com/Altinity/clickhouse-backup/issues/826
		if strings.Contains(info.Name(), "frozen_metadata") {
			return nil
		}

		// possible relative path
		// store / 1f9 / 1f9dc899-0de9-41f8-b95c-26c1f0d67d93 / 20181023_2_2_0 / checksums.txt
		// store / 1f9 / 1f9dc899-0de9-41f8-b95c-26c1f0d67d93 / 20181023_2_2_0 / x.proj / checksums.txt
		// data / database / table / 20181023_2_2_0 / checksums.txt
		// data / database / table / 20181023_2_2_0 / x.proj / checksums.txt
		relativePath := strings.Trim(strings.TrimPrefix(filePath, shadowPath), "/")
		pathParts := strings.SplitN(relativePath, "/", 4)
		if len(pathParts) != 4 {
			return nil
		}
		if len(partitionsBackupMap) != 0 && !IsPartInPartition(pathParts[3], partitionsBackupMap) {
			return nil
		}
		var isRequiredPartFound, partExists bool
		if tableDiffFromRemote.Database != "" && tableDiffFromRemote.Table != "" && len(tableDiffFromRemote.Parts) > 0 && len(tableDiffFromRemote.Parts[disk.Name]) > 0 {
			parts, isRequiredPartFound, partExists = addRequiredPartIfNotExists(parts, pathParts[3], tableDiffFromRemote, disk)
			if isRequiredPartFound {
				return nil
			}
		}
		dstFilePath := filepath.Join(backupPartsPath, pathParts[3])
		if info.IsDir() {
			if !strings.HasSuffix(pathParts[3], ".proj") && !isRequiredPartFound && !partExists {
				parts = append(parts, metadata.Part{
					Name: pathParts[3],
				})
			}
			return os.MkdirAll(dstFilePath, 0750)
		}
		if !info.Mode().IsRegular() {
			log.Debugf("'%s' is not a regular file, skipping", filePath)
			return nil
		}
		size += info.Size()
		if version < 21004000 {
			return os.Rename(filePath, dstFilePath)
		} else {
			return os.Link(filePath, dstFilePath)
		}
	})
	return parts, size, err
}

func addRequiredPartIfNotExists(parts []metadata.Part, relativePath string, tableDiffFromRemote metadata.TableMetadata, disk clickhouse.Disk) ([]metadata.Part, bool, bool) {
	isRequiredPartFound := false
	exists := false
	for _, diffPart := range tableDiffFromRemote.Parts[disk.Name] {
		if diffPart.Name == relativePath || strings.HasPrefix(relativePath, diffPart.Name+"/") {
			for _, p := range parts {
				if p.Name == relativePath || strings.HasPrefix(relativePath, p.Name+"/") {
					exists = true
					break
				}
			}
			if !exists {
				parts = append(parts, metadata.Part{
					Name:     relativePath,
					Required: true,
				})
			}
			isRequiredPartFound = true
		}
	}
	return parts, isRequiredPartFound, exists
}

func IsDuplicatedParts(part1, part2 string) error {
	log := apexLog.WithField("logger", "IsDuplicatedParts")
	p1, err := os.Open(part1)
	if err != nil {
		return err
	}
	defer func() {
		if err = p1.Close(); err != nil {
			log.Warnf("Can't close %s", part1)
		}
	}()
	p2, err := os.Open(part2)
	if err != nil {
		return err
	}
	defer func() {
		if err = p2.Close(); err != nil {
			log.Warnf("Can't close %s", part2)
		}
	}()
	pf1, err := p1.Readdirnames(-1)
	if err != nil {
		return err
	}
	pf2, err := p2.Readdirnames(-1)
	if err != nil {
		return err
	}
	if len(pf1) != len(pf2) {
		return fmt.Errorf("files count in parts is different")
	}
	for _, f := range pf1 {
		part1File, err := os.Stat(path.Join(part1, f))
		if err != nil {
			return err
		}
		part2File, err := os.Stat(path.Join(part2, f))
		if err != nil {
			return err
		}
		if !os.SameFile(part1File, part2File) {
			return fmt.Errorf("file '%s' is different", f)
		}
	}
	return nil
}
