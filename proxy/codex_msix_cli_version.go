package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"regexp"
)

const (
	codexPEHeaderBytes     = 64 << 10
	codexPEHeaderOffset    = 0x3c
	codexPEFileHeaderBytes = 24
	codexPESectionBytes    = 40
	codexPESectionLimit    = 96
	codexPEMachineX64      = 0x8664
	codexPEMachineARM64    = 0xaa64
	codexPESectionRead     = 0x40000000
	codexPESectionWrite    = 0x80000000
	codexMSIXCLIScanMax    = 8 << 20
	codexMSIXCLIScanStep   = 2 << 20
)

var codexRuntimeVersionMarker = regexp.MustCompile(`version: ([0-9][0-9A-Za-z.+-]{0,127})\nplatform: `)

type codexPESection struct {
	offset int64
	size   int64
}

func codexMSIXEmbeddedCLI(blocks *codexMSIXBlockMap, target string) (string, error) {
	file, err := blocks.open("app/resources/codex.exe")
	if err != nil {
		return "", err
	}
	header := make([]byte, min(file.size, codexPEHeaderBytes))
	if _, err := file.ReadAt(header, 0); err != nil {
		return "", err
	}
	section, err := codexPEReadOnlySection(header, target, file.size)
	if err != nil {
		return "", err
	}
	limit := min(section.size, codexMSIXCLIScanMax)
	data := make([]byte, 0, limit)
	for consumed := int64(0); consumed < limit; {
		chunk := make([]byte, min(codexMSIXCLIScanStep, limit-consumed))
		if _, err := file.ReadAt(chunk, section.offset+consumed); err != nil {
			return "", err
		}
		data, consumed = append(data, chunk...), consumed+int64(len(chunk))
		version, err := codexVersionFromPEMarkers(data)
		if err != nil || version != "" {
			return version, err
		}
	}
	return "", fmt.Errorf("unsupported MSIX CLI layout: verified version markers missing")
}

func codexPESectionTable(header []byte, target string) (int, int, error) {
	if len(header) < codexPEHeaderOffset+4 || string(header[:2]) != "MZ" {
		return 0, 0, fmt.Errorf("invalid CLI DOS header")
	}
	offset := int(binary.LittleEndian.Uint32(header[codexPEHeaderOffset:]))
	if offset > len(header)-codexPEFileHeaderBytes || string(header[offset:offset+4]) != "PE\x00\x00" {
		return 0, 0, fmt.Errorf("invalid CLI PE header")
	}
	machine := binary.LittleEndian.Uint16(header[offset+4:])
	expected := uint16(codexPEMachineX64)
	if target == "win32-arm64" {
		expected = codexPEMachineARM64
	}
	if machine != expected {
		return 0, 0, fmt.Errorf("MSIX CLI PE architecture mismatch")
	}
	count := int(binary.LittleEndian.Uint16(header[offset+6:]))
	start := offset + codexPEFileHeaderBytes + int(binary.LittleEndian.Uint16(header[offset+20:]))
	if count <= 0 || count > codexPESectionLimit || start > len(header)-count*codexPESectionBytes {
		return 0, 0, fmt.Errorf("invalid CLI PE section table")
	}
	return start, count, nil
}

func codexPEReadOnlySection(header []byte, target string, size int64) (codexPESection, error) {
	start, count, err := codexPESectionTable(header, target)
	if err != nil {
		return codexPESection{}, err
	}
	for index := 0; index < count; index++ {
		entry := header[start+index*codexPESectionBytes:][:codexPESectionBytes]
		if string(bytes.TrimRight(entry[:8], "\x00")) != ".rdata" {
			continue
		}
		section := codexPESection{size: int64(binary.LittleEndian.Uint32(entry[16:])), offset: int64(binary.LittleEndian.Uint32(entry[20:]))}
		flags := binary.LittleEndian.Uint32(entry[36:])
		if section.size <= 0 || section.offset < int64(start+count*codexPESectionBytes) || section.size > size-section.offset {
			return codexPESection{}, fmt.Errorf("invalid CLI read-only section bounds")
		}
		if flags&codexPESectionRead == 0 || flags&codexPESectionWrite != 0 {
			return codexPESection{}, fmt.Errorf("CLI version section is not read-only")
		}
		return section, nil
	}
	return codexPESection{}, fmt.Errorf("CLI read-only section missing")
}

func codexVersionFromPEMarkers(data []byte) (string, error) {
	version := ""
	for _, match := range codexRuntimeVersionMarker.FindAllSubmatch(data, -1) {
		candidate := string(match[1])
		if !validCodexClientVersionString(candidate) || !codexDoctorVersionMarker(data, candidate) {
			continue
		}
		if version != "" && version != candidate {
			return "", fmt.Errorf("conflicting compiled CLI version markers")
		}
		version = candidate
	}
	return version, nil
}

func codexDoctorVersionMarker(data []byte, version string) bool {
	prefix := []byte("codex-doctor/" + version)
	for offset := 0; offset < len(data); {
		index := bytes.Index(data[offset:], prefix)
		if index < 0 {
			return false
		}
		offset += index + len(prefix)
		if offset == len(data) {
			return false
		}
		if bytes.ContainsRune([]byte{0, ' ', '\r', '\n'}, rune(data[offset])) {
			return true
		}
		if bytes.HasPrefix(data[offset:], []byte(`cli\src\doctor\updates.rs`)) || bytes.HasPrefix(data[offset:], []byte("cli/src/doctor/updates.rs")) {
			return true
		}
	}
	return false
}

func codexMSIXBlockAppVersion(blocks *codexMSIXBlockMap) (string, error) {
	file, err := blocks.open("app/resources/app.asar")
	if err != nil {
		return "", err
	}
	target, size, _, err := codexASARPackageLocation(io.NewSectionReader(file, 0, file.size))
	if err != nil {
		return "", err
	}
	data := make([]byte, size)
	if _, err := file.ReadAt(data, target); err != nil {
		return "", err
	}
	return codexDesktopPackageVersion(data)
}
