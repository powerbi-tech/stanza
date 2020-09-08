package file

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/observiq/stanza/entry"
	"github.com/observiq/stanza/operator/helper"
	"go.uber.org/zap"
	"golang.org/x/text/encoding"
)

// InputOperator is an operator that monitors files for entries
type InputOperator struct {
	helper.InputOperator

	Include       []string
	Exclude       []string
	FilePathField entry.Field
	FileNameField entry.Field
	PollInterval  time.Duration
	SplitFunc     bufio.SplitFunc
	MaxLogSize    int

	persist helper.Persister

	knownFiles       map[string]*FileReader
	startAtBeginning bool

	fingerprintBytes int64

	encoding encoding.Encoding

	wg     *sync.WaitGroup
	cancel context.CancelFunc
}

// Start will start the file monitoring process
func (f *InputOperator) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.wg = &sync.WaitGroup{}

	// Load offsets from disk
	if err := f.loadKnownFiles(); err != nil {
		return fmt.Errorf("read known files from database: %s", err)
	}

	// Start polling goroutine
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		f.pollForNewFiles(ctx)
	}()

	return nil
}

// Stop will stop the file monitoring process
func (f *InputOperator) Stop() error {
	f.cancel()
	f.wg.Wait()
	f.syncKnownFiles()
	return nil
}

func (f *InputOperator) pollForNewFiles(ctx context.Context) {
	globTicker := time.NewTicker(f.PollInterval)
	defer globTicker.Stop()

	firstCheck := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-globTicker.C:
		}

		f.syncKnownFiles()
		// TODO clean unseen files from our list of known files. This grows unbound
		// if the files rotate
		matches := getMatches(f.Include, f.Exclude)
		if firstCheck && len(matches) == 0 {
			f.Warnw("no files match the configured include patterns", "include", f.Include)
		}
		for _, match := range matches {
			f.checkPath(ctx, match, firstCheck)
		}
		firstCheck = false
	}

}

func getMatches(includes, excludes []string) []string {
	all := make([]string, 0, len(includes))
	for _, include := range includes {
		matches, _ := filepath.Glob(include) // compile error checked in build
	INCLUDE:
		for _, match := range matches {
			for _, exclude := range excludes {
				if itMatches, _ := filepath.Match(exclude, match); itMatches {
					break INCLUDE
				}
			}

			for _, existing := range all {
				if existing == match {
					break INCLUDE
				}
			}

			all = append(all, match)
		}
	}

	return all
}

func (f *InputOperator) checkPath(ctx context.Context, path string, firstCheck bool) {
	// Check if we've seen this path before
	reader, ok := f.knownFiles[path]
	if !ok {
		// If we haven't seen it, create a new FileReader
		var err error
		reader, err = f.newFileReader(path, firstCheck)
		if err != nil {
			f.Errorw("Failed to create new reader", zap.Error(err))
			return
		}
		f.knownFiles[path] = reader
	}

	// Read to the end of the file
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		reader.ReadToEnd(ctx)
	}()
}

func (f *InputOperator) newFileReader(path string, firstCheck bool) (*FileReader, error) {
	newReader := NewFileReader(path, f)

	startAtBeginning := !firstCheck || f.startAtBeginning
	if err := newReader.Initialize(startAtBeginning); err != nil {
		return nil, err
	}

	// Check that this isn't a file we know about that has been moved or rotated
	for oldPath, reader := range f.knownFiles {
		reader.Lock()
		if newReader.Fingerprint.Matches(reader.Fingerprint) {
			// This file has been renamed, so update the path on the
			// old reader and use that instead
			reader.Path = path
			newReader = reader
			delete(f.knownFiles, oldPath)
			reader.Unlock()
			break
		}
		reader.Unlock()
	}

	f.knownFiles[path] = newReader
	return newReader, nil
}

var knownFilesKey = "knownFiles"

func (f *InputOperator) syncKnownFiles() {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	// Encode the number of known files
	if err := enc.Encode(len(f.knownFiles)); err != nil {
		f.Errorw("Failed to encode known files", zap.Error(err))
		return
	}

	// Encode each known file
	for _, fileReader := range f.knownFiles {
		if err := enc.Encode(fileReader); err != nil {
			f.Errorw("Failed to encode known files", zap.Error(err))
			return
		}
	}

	f.persist.Set(knownFilesKey, buf.Bytes())
	f.persist.Sync()
}

func (f *InputOperator) loadKnownFiles() error {
	err := f.persist.Load()
	if err != nil {
		return err
	}

	encoded := f.persist.Get(knownFilesKey)
	if encoded == nil {
		f.knownFiles = make(map[string]*FileReader)
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(encoded))

	// Decode the number of entries
	var knownFileCount int
	if err := dec.Decode(&knownFileCount); err != nil {
		return fmt.Errorf("decoding file count: %w", err)
	}

	// Decode each of the known files
	f.knownFiles = make(map[string]*FileReader)
	for i := 0; i < knownFileCount; i++ {
		newFileReader := NewFileReader("", f)
		if err = dec.Decode(newFileReader); err != nil {
			return err
		}
		f.knownFiles[newFileReader.Path] = newFileReader
	}

	return nil
}
