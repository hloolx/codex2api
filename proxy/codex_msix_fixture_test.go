package proxy

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"math/rand"
	"strings"
	"testing"
)

func codexTestMSIXCompressed(t *testing.T, data []byte) ([]byte, []codexMSIXBlock) {
	t.Helper()
	var compressed bytes.Buffer
	var blocks []codexMSIXBlock
	for start := 0; start < len(data); start += codexMSIXBlockBytes {
		block := data[start:min(start+codexMSIXBlockBytes, len(data))]
		var fragment bytes.Buffer
		writer, err := flate.NewWriter(&fragment, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(block); err != nil {
			t.Fatal(err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(block)
		blocks = append(blocks, codexMSIXBlock{Size: int64(fragment.Len()), Hash: base64.StdEncoding.EncodeToString(hash[:])})
		compressed.Write(fragment.Bytes())
		flushed := fragment.Len()
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if start+len(block) == len(data) {
			compressed.Write(fragment.Bytes()[flushed:])
		}
	}
	return compressed.Bytes(), blocks
}

func codexTestMSIXArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	var document struct {
		XMLName    xml.Name             `xml:"BlockMap"`
		HashMethod string               `xml:"HashMethod,attr"`
		Files      []codexMSIXBlockFile `xml:"File"`
	}
	document.HashMethod = codexMSIXHashMethod
	for name, content := range files {
		compressed, blocks := codexTestMSIXCompressed(t, content)
		file, err := writer.CreateRaw(&zip.FileHeader{Name: name, Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(content), CompressedSize64: uint64(len(compressed)), UncompressedSize64: uint64(len(content))})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(compressed); err != nil {
			t.Fatal(err)
		}
		document.Files = append(document.Files, codexMSIXBlockFile{Name: strings.ReplaceAll(name, "/", `\`), Size: int64(len(content)), Blocks: blocks})
	}
	metadata, err := xml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.Create("AppxBlockMap.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(metadata); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func codexTestMSIXPE(machine uint16, version string) []byte {
	const sectionOffset = 2 << 20
	data := make([]byte, sectionOffset+codexMSIXBlockBytes)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[codexPEHeaderOffset:], 128)
	copy(data[128:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[132:], machine)
	binary.LittleEndian.PutUint16(data[134:], 1)
	binary.LittleEndian.PutUint16(data[148:], 0)
	section := data[128+codexPEFileHeaderBytes:][:codexPESectionBytes]
	clear(section)
	copy(section, ".rdata")
	binary.LittleEndian.PutUint32(section[16:], codexMSIXBlockBytes)
	binary.LittleEndian.PutUint32(section[20:], sectionOffset)
	binary.LittleEndian.PutUint32(section[36:], codexPESectionRead)
	copy(data[sectionOffset:], "codex-doctor/"+version+`cli\src\doctor\updates.rs`+"\x00version: "+version+"\nplatform: ")
	return data
}

func codexTestMSIXSparseASAR(version string) []byte {
	const packageOffset = 16 << 20
	pkg := `{"version":"` + version + `"}`
	header := fmt.Sprintf(`{"files":{"package.json":{"offset":"%d","size":%d}}}`, packageOffset, len(pkg))
	data := make([]byte, 16+len(header)+packageOffset+len(pkg))
	_, _ = rand.New(rand.NewSource(2)).Read(data)
	clear(data[:16])
	binary.LittleEndian.PutUint32(data[4:], uint32(8+len(header)))
	binary.LittleEndian.PutUint32(data[12:], uint32(len(header)))
	copy(data[16:], header)
	copy(data[16+len(header)+packageOffset:], pkg)
	return data
}
