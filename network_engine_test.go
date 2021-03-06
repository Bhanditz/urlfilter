package urlfilter

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/process"
	"github.com/stretchr/testify/assert"
)

const (
	testResourcesDir = "test"
	filterPath       = testResourcesDir + "/easylist.txt"
	requestsPath     = testResourcesDir + "/requests.json"
)

type testRequest struct {
	Line        string
	URL         string `json:"url"`
	FrameUrl    string `json:"frameUrl"`
	RequestType string `json:"cpt"`
}

func TestEmptyNetworkEngine(t *testing.T) {
	engine := NewNetworkEngine(make([]*NetworkRule, 0))
	r := NewRequest("http://example.org/", "", TypeOther)
	rule, ok := engine.Match(r)
	assert.False(t, ok)
	assert.Nil(t, rule)
}

func TestBenchNetworkEngine(t *testing.T) {
	testRequests := loadRequests(t)
	assert.True(t, len(testRequests) > 0)
	var requests []*Request
	for _, req := range testRequests {
		r := NewRequest(req.URL, req.FrameUrl, getRequestType(req.RequestType))
		requests = append(requests, r)
	}

	start := getRSS()
	log.Printf("RSS before loading rules - %d kB\n", start/1024)

	startParse := time.Now()
	engine := buildNetworkEngine(t)
	assert.NotNil(t, engine)
	log.Printf("Elapsed on parsing rules: %v", time.Since(startParse))

	afterLoad := getRSS()
	log.Printf("RSS after loading rules - %d kB (%d kB diff)\n", afterLoad/1024, (afterLoad-start)/1024)

	totalMatches := 0
	totalElapsed := time.Duration(0)
	minElapsedMatch := time.Hour
	maxElapsedMatch := time.Duration(0)

	for i, req := range requests {
		if i != 0 && i%10000 == 0 {
			log.Printf("Processed %d requests", i)
		}

		startMatch := time.Now()
		rule, ok := engine.Match(req)
		elapsedMatch := time.Since(startMatch)
		totalElapsed += elapsedMatch
		if elapsedMatch > maxElapsedMatch {
			maxElapsedMatch = elapsedMatch
		}
		if elapsedMatch < minElapsedMatch {
			minElapsedMatch = elapsedMatch
		}

		if ok && !rule.Whitelist {
			totalMatches++
		}
	}

	log.Printf("Total matches: %d", totalMatches)
	log.Printf("Total elapsed: %v", totalElapsed)
	log.Printf("Average per request: %v", time.Duration(int64(totalElapsed)/int64(len(requests))))
	log.Printf("Max per request: %v", maxElapsedMatch)
	log.Printf("Min per request: %v", minElapsedMatch)

	afterMatch := getRSS()
	log.Printf("RSS after matching - %d kB (%d kB diff)\n", afterMatch/1024, (afterMatch-afterLoad)/1024)
}

// getRequestType converts string value from requests.json to RequestType
// This maps puppeteer types to WebRequest types
func getRequestType(t string) RequestType {
	switch t {
	case "document":
		// Consider document requests as sub_document. This is because the request
		// dataset does not contain sub_frame or main_frame but only 'document'.
		return TypeSubdocument
	case "stylesheet":
		return TypeStylesheet
	case "font":
		return TypeFont
	case "image":
		return TypeImage
	case "media":
		return TypeMedia
	case "script":
		return TypeScript
	case "xhr", "fetch":
		return TypeXmlhttprequest
	case "websocket":
		return TypeWebsocket
	default:
		return TypeOther
	}
}

func isSupportedURL(url string) bool {
	return url != "" && (strings.HasPrefix(url, "http") ||
		strings.HasPrefix(url, "ws"))
}

func buildNetworkEngine(t *testing.T) *NetworkEngine {
	file, err := os.Open(filterPath)
	if err != nil {
		t.Fatalf("cannot load %s: %s", filterPath, err)
	}
	defer file.Close()

	var rules []*NetworkRule

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line != "" && !isCosmetic(line) && !isComment(line) {

			rule, err := NewNetworkRule(line, 0)
			if err == nil {
				rules = append(rules, rule)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	log.Printf("Loaded %d rules from %s", len(rules), filterPath)
	return NewNetworkEngine(rules)
}

func loadRequests(t *testing.T) []testRequest {
	if _, err := os.Stat(requestsPath); os.IsNotExist(err) {
		err = unzip(requestsPath+".zip", testResourcesDir)
		if err != nil {
			t.Fatalf("cannot unzip %s.zip", requestsPath)
		}
	}

	file, err := os.Open(requestsPath)
	if err != nil {
		t.Fatalf("cannot load %s: %s", requestsPath, err)
	}
	defer file.Close()

	var requests []testRequest

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			var req testRequest
			err := json.Unmarshal([]byte(line), &req)
			if err == nil && isSupportedURL(req.URL) && isSupportedURL(req.FrameUrl) {
				req.Line = line
				requests = append(requests, req)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	log.Printf("Loaded %d requests from %s", len(requests), requestsPath)
	return requests
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer func() {
		if err := r.Close(); err != nil {
			panic(err)
		}
	}()

	os.MkdirAll(dest, 0755)

	// Closure to address file descriptors issue with all the deferred .Close() methods
	extractAndWriteFile := func(f *zip.File) error {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer func() {
			if err := rc.Close(); err != nil {
				panic(err)
			}
		}()

		path := filepath.Join(dest, f.Name)

		if f.FileInfo().IsDir() {
			os.MkdirAll(path, f.Mode())
		} else {
			os.MkdirAll(filepath.Dir(path), f.Mode())
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				return err
			}
			defer func() {
				if err := f.Close(); err != nil {
					panic(err)
				}
			}()

			_, err = io.Copy(f, rc)
			if err != nil {
				return err
			}
		}
		return nil
	}

	for _, f := range r.File {
		err := extractAndWriteFile(f)
		if err != nil {
			return err
		}
	}

	return nil
}

func getRSS() uint64 {
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		panic(err)
	}
	minfo, err := proc.MemoryInfo()
	if err != nil {
		panic(err)
	}
	return minfo.RSS
}
