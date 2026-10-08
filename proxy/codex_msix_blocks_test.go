package proxy

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

func TestCodexMSIXBlockFallbackReadsSparseVersionPair(t *testing.T) {
	files := map[string][]byte{
		"AppxManifest.xml":        []byte(`<Package><Identity Name="OpenAI.Codex" Version="26.928.3736.0" ProcessorArchitecture="x64"/></Package>`),
		"app/resources/app.asar":  codexTestMSIXSparseASAR("26.928.31416"),
		"app/resources/codex.exe": codexTestMSIXPE(codexPEMachineX64, "0.158.0-alpha.2.1"),
	}
	data := codexTestMSIXArchive(t, files)
	server := codexTestRangeServer(t, data)
	remote, err := openCodexRemoteArchive(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	candidate := codexWindowsCandidate(codexWindowsUpdate{BuildVersion: "26.928.3736.0"}, "x64")
	pair, err := resolveCodexMSIXPair(remote, candidate)
	if err != nil || pair.AppVersion != "26.928.31416" || pair.CLIVersion != "0.158.0-alpha.2.1" || pair.Source != "official_msix_blockmap" {
		t.Fatalf("block pair: %+v, %v", pair, err)
	}
	if remote.ranges.used >= int64(len(data))/2 || remote.ranges.used > codexMSIXBudgetBytes {
		t.Fatalf("fallback read sparse payload: %d / %d", remote.ranges.used, len(data))
	}
	if _, err := codexMSIXEmbeddedCLI(mustCodexTestMSIXBlocks(t, remote), "win32-arm64"); err == nil {
		t.Fatal("wrong PE architecture accepted")
	}
}

func mustCodexTestMSIXBlocks(t *testing.T, remote *codexRemoteArchive) *codexMSIXBlockMap {
	t.Helper()
	blocks, err := newCodexMSIXBlockMap(remote)
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func TestCodexMSIXBlockHashRejectsCorruption(t *testing.T) {
	data := codexTestMSIXArchive(t, map[string][]byte{"payload": []byte(strings.Repeat("valid", codexMSIXBlockBytes))})
	server := codexTestRangeServer(t, data)
	remote, err := openCodexRemoteArchive(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	blocks := mustCodexTestMSIXBlocks(t, remote)
	metadata := blocks.files["payload"]
	metadata.Blocks[1].Hash = base64.StdEncoding.EncodeToString(make([]byte, 32))
	blocks.files["payload"] = metadata
	file, err := blocks.open("payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.ReadAt(make([]byte, 10), codexMSIXBlockBytes); err == nil {
		t.Fatal("corrupted block hash accepted")
	}
}

func TestCodexMSIXBlockMapRejectsInvalidLengthsAndHashes(t *testing.T) {
	for _, kind := range []string{"size", "count", "compression", "hash", "compressed_size"} {
		t.Run(kind, func(t *testing.T) {
			metadata := codexMSIXBlockFile{Size: 1, Blocks: []codexMSIXBlock{{Size: 10, Hash: base64.StdEncoding.EncodeToString(make([]byte, 32))}}}
			file := &zip.File{FileHeader: zip.FileHeader{Method: zip.Deflate, UncompressedSize64: 1}}
			switch kind {
			case "size":
				metadata.Size = 2
			case "count":
				metadata.Blocks = nil
			case "compression":
				file.Method = 99
			case "hash":
				metadata.Blocks[0].Hash = "invalid"
			case "compressed_size":
				metadata.Blocks[0].Size = 0
			}
			err := validateCodexMSIXBlockFile(file, metadata)
			if err == nil {
				_, err = codexMSIXReadBlocks(metadata, file.Method)
			}
			if err == nil {
				t.Fatal("invalid block map accepted")
			}
		})
	}
}

func TestCodexPEVersionMarkersRequireAgreementAndPreserveAlpha(t *testing.T) {
	for _, version := range []string{"0.159.2", "0.158.0-alpha.2.1"} {
		data := []byte("codex-doctor/" + version + "\x00version: " + version + "\nplatform: ")
		got, err := codexVersionFromPEMarkers(data)
		if got != version || err != nil {
			t.Fatalf("marker: %q, %v", got, err)
		}
	}
	for _, data := range []string{"random 0.159.2", "codex-doctor/0.159.2\x00", "version: 0.159.2\nplatform: ", "codex-doctor/0.159.20\x00version: 0.159.2\nplatform: "} {
		got, err := codexVersionFromPEMarkers([]byte(data))
		if got != "" || err != nil {
			t.Fatalf("unverified marker: %q, %v", got, err)
		}
	}
	conflict := []byte("codex-doctor/0.159.2\x00version: 0.159.2\nplatform: codex-doctor/0.158.0\x00version: 0.158.0\nplatform: ")
	if _, err := codexVersionFromPEMarkers(conflict); err == nil {
		t.Fatal("conflicting markers accepted")
	}
}
