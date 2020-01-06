package checksum

import (
	"hash/crc32"
	"io"
	"os"
	"sync"

	"github.com/vimeo/go-util/crc32combine"
	"golang.org/x/exp/mmap"
)

// CRC32CChecksum computes the crc32c checksum of a file
func CRC32CChecksum(data []byte) (uint32, error) {
	// This ensures that we use the crc32c system command if available
	//   I stepped though the code to verify
	crc32q := crc32.MakeTable(crc32.Castagnoli)
	return crc32.Checksum(data, crc32q), nil
}

type partRange struct {
	Chunk int64
	Start int64
	End   int64
}

type partChecksum struct {
	Chunk    int64
	Checksum uint32
	Error    error
}

func checksumWorker(readerAt *io.ReaderAt, partRanges <-chan partRange, checksums chan<- partChecksum) {
	for partRange := range partRanges {
		data := make([]byte, partRange.End-partRange.Start)
		(*readerAt).ReadAt(data, partRange.Start)
		checksum, err := CRC32CChecksum(data)
		checksums <- partChecksum{
			Chunk:    partRange.Chunk,
			Checksum: checksum,
			Error:    err,
		}
	}
}

func parallelCRCFuse(checksums *[]uint32, numParts, partSize, length, lastPartSize int64) uint32 {
	nextPower := numParts << 1
	for n := int64(1); n < nextPower; n <<= 1 {
		var wg sync.WaitGroup

		for i := int64(0); i+n < numParts; i += 2 * n {
			wg.Add(1)
			go func(i int64) {
				len2 := partSize * n
				prevLen := (i + n) * partSize
				if len2+prevLen > length {
					len2 = length - prevLen
				} else if i+n == numParts-n {
					len2 -= (partSize - lastPartSize)
				}
				(*checksums)[i] = crc32combine.CRC32Combine(crc32.Castagnoli, (*checksums)[i], (*checksums)[i+n], len2)
				wg.Done()
			}(i)
		}
		wg.Wait()
	}
	return (*checksums)[0]
}

// ParallelCRC32CChecksum a
func ParallelCRC32CChecksum(filename string, partSize int64, concurrency int, useMmap bool) (uint32, error) {
	stats, err := os.Stat(filename)
	if err != nil {
		return 0, err
	}
	length := stats.Size()

	var readerAt io.ReaderAt
	if useMmap {
		readerAt, err = mmap.Open(filename)
	} else {
		readerAt, err = os.Open(filename)
	}
	if err != nil {
		return 0, err
	}

	numParts := length / partSize
	lastPartSize := length % partSize
	if lastPartSize > 0 {
		numParts++
	} else {
		lastPartSize = partSize
	}

	partRanges := make(chan partRange, numParts)
	partChecksums := make(chan partChecksum, numParts)
	checksums := make([]uint32, numParts)

	for w := 0; w < concurrency; w++ {
		go checksumWorker(&readerAt, partRanges, partChecksums)
	}

	for i := int64(0); i < numParts-1; i++ {
		partRanges <- partRange{
			Chunk: i,
			Start: i * partSize,
			End:   (i + 1) * partSize,
		}
	}

	partRanges <- partRange{
		Chunk: numParts - 1,
		Start: (numParts - 1) * partSize,
		End:   length,
	}

	close(partRanges)

	for i := int64(0); i < numParts; i++ {
		partChecksum := <-partChecksums
		if partChecksum.Error != nil {
			return 0, partChecksum.Error
		}
		checksums[partChecksum.Chunk] = partChecksum.Checksum
	}

	checksum := parallelCRCFuse(&checksums, numParts, partSize, length, lastPartSize)

	return checksum, nil
}
