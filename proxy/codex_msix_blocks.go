package proxy

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	codexMSIXBlockBytes     = 64 << 10
	codexMSIXBlockMapMax    = 8 << 20
	codexMSIXDeflateTailMax = 6
	codexMSIXHashMethod     = "http://www.w3.org/2001/04/xmlenc#sha256"
)

type codexMSIXBlock struct {
	Hash string `xml:"Hash,attr"`
	Size int64  `xml:"Size,attr"`
}

type codexMSIXBlockFile struct {
	Name   string           `xml:"Name,attr"`
	Size   int64            `xml:"Size,attr"`
	Blocks []codexMSIXBlock `xml:"Block"`
}

type codexMSIXBlockMap struct {
	remote *codexRemoteArchive
	files  map[string]codexMSIXBlockFile
}

func newCodexMSIXBlockMap(remote *codexRemoteArchive) (*codexMSIXBlockMap, error) {
	data, err := codexArchiveFileLimited(remote.archive, "AppxBlockMap.xml", codexMSIXBlockMapMax)
	if err != nil {
		return nil, err
	}
	var document struct {
		XMLName    xml.Name             `xml:"BlockMap"`
		HashMethod string               `xml:"HashMethod,attr"`
		Files      []codexMSIXBlockFile `xml:"File"`
	}
	if err := xml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if document.HashMethod != codexMSIXHashMethod {
		return nil, fmt.Errorf("unsupported MSIX block hash method")
	}
	result := &codexMSIXBlockMap{remote: remote, files: make(map[string]codexMSIXBlockFile)}
	for _, file := range document.Files {
		name := strings.ReplaceAll(file.Name, `\`, "/")
		if _, exists := result.files[name]; exists {
			return nil, fmt.Errorf("duplicate MSIX block map file")
		}
		result.files[name] = file
	}
	return result, nil
}

type codexMSIXReadBlock struct {
	offset int64
	size   int64
	hash   [sha256.Size]byte
}

type codexMSIXFileReader struct {
	source io.ReaderAt
	size   int64
	method uint16
	blocks []codexMSIXReadBlock
	cache  map[int][]byte
}

func (m *codexMSIXBlockMap) open(name string) (*codexMSIXFileReader, error) {
	metadata, exists := m.files[name]
	if !exists {
		return nil, fmt.Errorf("MSIX block map file missing: %s", name)
	}
	file, err := codexMSIXZIPFile(m.remote.archive, name)
	if err != nil {
		return nil, err
	}
	if err := validateCodexMSIXBlockFile(file, metadata); err != nil {
		return nil, err
	}
	offset, err := file.DataOffset()
	if err != nil {
		return nil, err
	}
	blocks, err := codexMSIXReadBlocks(metadata, file.Method)
	if err != nil {
		return nil, err
	}
	last := blocks[len(blocks)-1]
	compressed := last.offset + last.size
	if compressed > int64(file.CompressedSize64) || int64(file.CompressedSize64)-compressed > codexMSIXDeflateTailMax {
		return nil, fmt.Errorf("MSIX compressed block sizes mismatch")
	}
	return &codexMSIXFileReader{source: io.NewSectionReader(m.remote.ranges, offset, int64(file.CompressedSize64)), size: metadata.Size, method: file.Method, blocks: blocks, cache: make(map[int][]byte)}, nil
}

func codexMSIXZIPFile(archive *zip.Reader, name string) (*zip.File, error) {
	var found *zip.File
	for _, file := range archive.File {
		if file.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("duplicate MSIX ZIP file")
		}
		found = file
	}
	if found == nil {
		return nil, fmt.Errorf("MSIX ZIP file missing: %s", name)
	}
	return found, nil
}

func validateCodexMSIXBlockFile(file *zip.File, metadata codexMSIXBlockFile) error {
	if metadata.Size <= 0 || uint64(metadata.Size) != file.UncompressedSize64 {
		return fmt.Errorf("MSIX block map file size mismatch")
	}
	if file.Method != zip.Deflate && file.Method != zip.Store {
		return fmt.Errorf("unsupported MSIX block compression")
	}
	if int64(len(metadata.Blocks)) != (metadata.Size-1)/codexMSIXBlockBytes+1 {
		return fmt.Errorf("MSIX block map block count mismatch")
	}
	return nil
}

func codexMSIXReadBlocks(metadata codexMSIXBlockFile, method uint16) ([]codexMSIXReadBlock, error) {
	blocks := make([]codexMSIXReadBlock, len(metadata.Blocks))
	offset := int64(0)
	for index, block := range metadata.Blocks {
		size := block.Size
		if method == zip.Store {
			size = min(codexMSIXBlockBytes, metadata.Size-int64(index)*codexMSIXBlockBytes)
		}
		if size <= 0 || size > 2*codexMSIXBlockBytes {
			return nil, fmt.Errorf("invalid MSIX compressed block size")
		}
		hash, err := base64.StdEncoding.DecodeString(block.Hash)
		if err != nil || len(hash) != sha256.Size {
			return nil, fmt.Errorf("invalid MSIX block hash")
		}
		blocks[index] = codexMSIXReadBlock{offset: offset, size: size, hash: [sha256.Size]byte(hash)}
		offset += size
	}
	return blocks, nil
}

func (r *codexMSIXFileReader) readBlock(index int) ([]byte, error) {
	if data, exists := r.cache[index]; exists {
		return data, nil
	}
	block := r.blocks[index]
	compressed := make([]byte, block.size)
	if _, err := r.source.ReadAt(compressed, block.offset); err != nil {
		return nil, err
	}
	data := compressed
	if r.method == zip.Deflate {
		stream := flate.NewReader(bytes.NewReader(compressed))
		var err error
		data, err = io.ReadAll(io.LimitReader(stream, codexMSIXBlockBytes+1))
		_ = stream.Close()
		// MSIX 的非末块不带 DEFLATE 结束标志，仍须校验长度和 SHA-256。
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
	}
	expected := min(codexMSIXBlockBytes, r.size-int64(index)*codexMSIXBlockBytes)
	if int64(len(data)) != expected || sha256.Sum256(data) != block.hash {
		return nil, fmt.Errorf("MSIX block length/hash mismatch")
	}
	r.cache[index] = data
	return data, nil
}

func (r *codexMSIXFileReader) ReadAt(data []byte, offset int64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if offset < 0 || offset >= r.size {
		return 0, io.EOF
	}
	read := 0
	for len(data) > 0 && offset < r.size {
		index := int(offset / codexMSIXBlockBytes)
		block, err := r.readBlock(index)
		if err != nil {
			return read, err
		}
		copied := copy(data, block[offset%codexMSIXBlockBytes:])
		read, offset, data = read+copied, offset+int64(copied), data[copied:]
	}
	if len(data) > 0 {
		return read, io.EOF
	}
	return read, nil
}
