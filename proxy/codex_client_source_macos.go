package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type codexAppcastItem struct {
	AppVersion string `xml:"shortVersionString"`
	Build      string `xml:"version"`
	Channel    string `xml:"channel"`
	Enclosure  struct {
		URL       string `xml:"url,attr"`
		Length    string `xml:"length,attr"`
		Signature string `xml:"edSignature,attr"`
	} `xml:"enclosure"`
}

func fetchCodexMacCandidate(ctx context.Context, client *http.Client, target string) codexClientCandidate {
	candidate := codexClientCandidate{Kind: string(CodexClientKindDesktop), Target: target}
	endpoint := codexClientSources.MacARM64
	if target == "darwin-x64" {
		endpoint = codexClientSources.MacX64
	}
	data, err := fetchCodexSmallJSON(ctx, client, endpoint)
	if err != nil {
		candidate.Err = err
		return candidate
	}
	var appcast struct {
		Items []codexAppcastItem `xml:"channel>item"`
	}
	if err := xml.Unmarshal(data, &appcast); err != nil {
		candidate.Err = err
		return candidate
	}
	best := newestCodexAppcastItem(appcast.Items)
	if best.AppVersion == "" || best.Build == "" {
		candidate.Err = fmt.Errorf("no valid stable package in macOS appcast")
		return candidate
	}
	candidate.NativeBuild = best.Build
	candidate.Pair = CodexClientVersionPair{AppVersion: best.AppVersion, Source: "official_appcast", ArtifactURL: best.Enclosure.URL,
		ArtifactID: codexArtifactIdentity(target, best.Enclosure.URL, best.Build, best.Enclosure.Length, best.Enclosure.Signature)}
	return candidate
}

func codexOuterPlist(archive *zip.Reader) (string, error) {
	name := ""
	for _, file := range archive.File {
		if !strings.HasSuffix(file.Name, ".app/Contents/Info.plist") || strings.Count(file.Name, ".app/") != 1 {
			continue
		}
		if name != "" {
			return "", fmt.Errorf("ambiguous macOS application plist")
		}
		name = file.Name
	}
	if name == "" {
		return "", fmt.Errorf("macOS application plist missing")
	}
	return name, nil
}

func codexPlistStrings(data []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	result := make(map[string]string)
	key := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return nil, err
			}
		case "string":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return nil, err
			}
			result[key], key = value, ""
		}
	}
}

func resolveCodexMacPair(archive *zip.Reader, candidate codexClientCandidate) (CodexClientVersionPair, error) {
	pair := candidate.Pair
	name, err := codexOuterPlist(archive)
	if err != nil {
		return pair, err
	}
	data, err := codexArchiveFile(archive, name)
	if err != nil {
		return pair, err
	}
	plist, err := codexPlistStrings(data)
	if err != nil {
		return pair, err
	}
	if plist["CFBundleIdentifier"] != "com.openai.codex" || plist["CFBundleShortVersionString"] != pair.AppVersion || plist["CFBundleVersion"] != candidate.NativeBuild {
		return pair, fmt.Errorf("macOS appcast and package version mismatch")
	}
	prefix := strings.TrimSuffix(name, "Info.plist") + "Resources/codex-cli/"
	pair.CLIVersion, err = codexArchiveCLI(archive, candidate.Target, prefix)
	return pair, err
}

func newestCodexAppcastItem(items []codexAppcastItem) codexAppcastItem {
	var best codexAppcastItem
	for _, item := range items {
		if item.Channel != "" && item.Channel != "stable" {
			continue
		}
		if !codexTrustedArtifactURL(item.Enclosure.URL) || !strings.HasSuffix(item.Enclosure.URL, ".zip") {
			continue
		}
		if codexBuildIsNewer(item.AppVersion, best.AppVersion) {
			best = item
		}
	}
	return best
}
