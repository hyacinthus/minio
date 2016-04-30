/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"io"
	slashpath "path"
	"time"

	"github.com/Sirupsen/logrus"
)

// Erasure block size.
const erasureBlockSize = 4 * 1024 * 1024 // 4MiB.

// cleanupCreateFileOps - cleans up all the temporary files and other
// temporary data upon any failure.
func (xl XL) cleanupCreateFileOps(volume, path string, writers ...io.WriteCloser) {
	closeAndRemoveWriters(writers...)
	for _, disk := range xl.storageDisks {
		if err := disk.DeleteFile(volume, path); err != nil {
			log.WithFields(logrus.Fields{
				"volume": volume,
				"path":   path,
			}).Errorf("DeleteFile failed with %s", err)
		}
	}
}

// Close and remove writers if they are safeFile.
func closeAndRemoveWriters(writers ...io.WriteCloser) {
	for _, writer := range writers {
		if err := safeCloseAndRemove(writer); err != nil {
			log.Errorf("Closing writer failed with %s", err)
		}
	}
}

// WriteErasure reads predefined blocks, encodes them and writes to
// configured storage disks.
func (xl XL) writeErasure(volume, path string, reader *io.PipeReader, wcloser *waitCloser) {
	// Release the block writer upon function return.
	defer wcloser.release()

	// Lock right before reading from disk.
	nsMutex.RLock(volume, path)
	partsMetadata, errs := xl.getPartsMetadata(volume, path)
	nsMutex.RUnlock(volume, path)

	// Count errors other than fileNotFound, bigger than the allowed
	// readQuorum, if yes throw an error.
	metadataReadErrCount := 0
	for _, err := range errs {
		if err != nil && err != errFileNotFound {
			metadataReadErrCount++
			if metadataReadErrCount > xl.readQuorum {
				log.WithFields(logrus.Fields{
					"volume": volume,
					"path":   path,
				}).Errorf("%s", err)
				reader.CloseWithError(err)
				return
			}
		}
	}

	var err error
	// List all the file versions on existing files.
	versions := listFileVersions(partsMetadata, errs)
	// Get highest file version.
	higherVersion := highestInt(versions)
	// Increment to have next higher version.
	higherVersion++

	writers := make([]io.WriteCloser, len(xl.storageDisks))

	xlMetaV1FilePath := slashpath.Join(path, xlMetaV1File)
	metadataWriters := make([]io.WriteCloser, len(xl.storageDisks))

	// Save additional erasureMetadata.
	modTime := time.Now().UTC()

	createFileError := 0
	for index, disk := range xl.storageDisks {
		erasurePart := slashpath.Join(path, fmt.Sprintf("file.%d", index))
		var writer io.WriteCloser
		writer, err = disk.CreateFile(volume, erasurePart)
		if err != nil {
			log.WithFields(logrus.Fields{
				"volume": volume,
				"path":   path,
			}).Errorf("CreateFile failed with %s", err)
			createFileError++

			// We can safely allow CreateFile errors up to len(xl.storageDisks) - xl.writeQuorum
			// otherwise return failure.
			if createFileError <= len(xl.storageDisks)-xl.writeQuorum {
				continue
			}

			// Remove previous temp writers for any failure.
			xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
			reader.CloseWithError(errWriteQuorum)
			return
		}

		// create meta data file
		var metadataWriter io.WriteCloser
		metadataWriter, err = disk.CreateFile(volume, xlMetaV1FilePath)
		if err != nil {
			log.WithFields(logrus.Fields{
				"volume": volume,
				"path":   path,
			}).Errorf("CreateFile failed with %s", err)
			createFileError++

			// We can safely allow CreateFile errors up to
			// len(xl.storageDisks) - xl.writeQuorum otherwise return failure.
			if createFileError <= len(xl.storageDisks)-xl.writeQuorum {
				continue
			}

			// Remove previous temp writers for any failure.
			xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
			reader.CloseWithError(errWriteQuorum)
			return
		}

		writers[index] = writer
		metadataWriters[index] = metadataWriter
	}

	// Allocate 4MiB block size buffer for reading.
	dataBuffer := make([]byte, erasureBlockSize)
	var totalSize int64 // Saves total incoming stream size.
	for {
		// Read up to allocated block size.
		var n int
		n, err = io.ReadFull(reader, dataBuffer)
		if err != nil {
			// Any unexpected errors, close the pipe reader with error.
			if err != io.ErrUnexpectedEOF && err != io.EOF {
				log.WithFields(logrus.Fields{
					"volume": volume,
					"path":   path,
				}).Errorf("io.ReadFull failed with %s", err)
				// Remove all temp writers.
				xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
				reader.CloseWithError(err)
				return
			}
		}
		// At EOF break out.
		if err == io.EOF {
			break
		}
		if n > 0 {
			// Split the input buffer into data and parity blocks.
			var dataBlocks [][]byte
			dataBlocks, err = xl.ReedSolomon.Split(dataBuffer[0:n])
			if err != nil {
				log.WithFields(logrus.Fields{
					"volume": volume,
					"path":   path,
				}).Errorf("Splitting data buffer into erasure data blocks failed with %s", err)
				// Remove all temp writers.
				xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
				reader.CloseWithError(err)
				return
			}

			// Encode parity blocks using data blocks.
			err = xl.ReedSolomon.Encode(dataBlocks)
			if err != nil {
				log.WithFields(logrus.Fields{
					"volume": volume,
					"path":   path,
				}).Errorf("Encoding erasure data blocks failed with %s", err)
				// Remove all temp writers upon error.
				xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
				reader.CloseWithError(err)
				return
			}

			// Loop through and write encoded data to quorum disks.
			for index, writer := range writers {
				if writer == nil {
					continue
				}
				encodedData := dataBlocks[index]
				_, err = writers[index].Write(encodedData)
				if err != nil {
					log.WithFields(logrus.Fields{
						"volume":    volume,
						"path":      path,
						"diskIndex": index,
					}).Errorf("Writing encoded blocks failed with %s", err)
					// Remove all temp writers upon error.
					xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
					reader.CloseWithError(err)
					return
				}
			}

			// Update total written.
			totalSize += int64(n)
		}
	}

	// Initialize metadata map, save all erasure related metadata.
	metadata := xlMetaV1{}
	metadata.Version = "1"
	metadata.Stat.Size = totalSize
	metadata.Stat.ModTime = modTime
	metadata.Minio.Release = minioReleaseTag
	if len(xl.storageDisks) > len(writers) {
		// Save file.version only if we wrote to less disks than all
		// storage disks.
		metadata.Stat.Version = higherVersion
	}
	metadata.Erasure.DataBlocks = xl.DataBlocks
	metadata.Erasure.ParityBlocks = xl.ParityBlocks
	metadata.Erasure.BlockSize = erasureBlockSize

	// Write all the metadata.
	// below case is not handled here
	// Case: when storageDisks is 16 and write quorumDisks is 13,
	//       meta data write failure up to 2 can be considered.
	//       currently we fail for any meta data writes
	for index, metadataWriter := range metadataWriters {
		if metadataWriter == nil {
			continue
		}

		// Write metadata.
		err = metadata.Write(metadataWriter)
		if err != nil {
			log.WithFields(logrus.Fields{
				"volume":    volume,
				"path":      path,
				"diskIndex": index,
			}).Errorf("Writing metadata failed with %s", err)
			// Remove temporary files.
			xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
			reader.CloseWithError(err)
			return
		}
	}

	// Lock right before commit to disk.
	nsMutex.Lock(volume, path)
	defer nsMutex.Unlock(volume, path)

	// Close all writers and metadata writers in routines.
	for index, writer := range writers {
		if writer == nil {
			continue
		}
		// Safely wrote, now rename to its actual location.
		if err = writer.Close(); err != nil {
			log.WithFields(logrus.Fields{
				"volume":    volume,
				"path":      path,
				"diskIndex": index,
			}).Errorf("Safely committing part failed with %s", err)
			// Remove all temp writers upon error.
			xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
			reader.CloseWithError(err)
			return
		}

		if metadataWriters[index] == nil {
			continue
		}
		// Safely wrote, now rename to its actual location.
		if err = metadataWriters[index].Close(); err != nil {
			log.WithFields(logrus.Fields{
				"volume":    volume,
				"path":      path,
				"diskIndex": index,
			}).Errorf("Safely committing metadata failed with %s", err)
			// Remove all temp writers upon error.
			xl.cleanupCreateFileOps(volume, path, append(writers, metadataWriters...)...)
			reader.CloseWithError(err)
			return
		}

	}

	// Close the pipe reader and return.
	reader.Close()
	return
}

// CreateFile - create a file.
func (xl XL) CreateFile(volume, path string) (writeCloser io.WriteCloser, err error) {
	if !isValidVolname(volume) {
		return nil, errInvalidArgument
	}
	if !isValidPath(path) {
		return nil, errInvalidArgument
	}

	// Initialize pipe for data pipe line.
	pipeReader, pipeWriter := io.Pipe()

	// Initialize a new wait closer, implements both Write and Close.
	wcloser := newWaitCloser(pipeWriter)

	// Start erasure encoding in routine, reading data block by block from pipeReader.
	go xl.writeErasure(volume, path, pipeReader, wcloser)

	// Return the writer, caller should start writing to this.
	return wcloser, nil
}
