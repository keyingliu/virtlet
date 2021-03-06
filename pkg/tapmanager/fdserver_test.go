/*
Copyright 2017 Mirantis

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tapmanager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
)

type sampleFDData struct {
	Content string
}

type sampleFDSource struct {
	tmpDir  string
	files   map[string]*os.File
	stopped bool
}

var _ FDSource = &sampleFDSource{}

func newSampleFDSource(tmpDir string) *sampleFDSource {
	return &sampleFDSource{
		tmpDir: tmpDir,
		files:  make(map[string]*os.File),
	}
}

func (s *sampleFDSource) GetFDs(key string, data []byte) ([]int, []byte, error) {
	if s.stopped {
		return nil, nil, errors.New("sampleFDSource is stopped")
	}

	var fdData sampleFDData
	if err := json.Unmarshal(data, &fdData); err != nil {
		return nil, nil, fmt.Errorf("error unmarshalling json: %v", err)
	}
	if _, found := s.files[key]; found {
		return nil, nil, fmt.Errorf("file already exists: %q", key)
	}
	filename := filepath.Join(s.tmpDir, key)
	f, err := os.Create(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating file %q: %v", filename, err)
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("Remove(): %v", err)
	}
	if _, err := f.Write([]byte(fdData.Content)); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("Write(): %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("Seek(): %v", err)
	}
	s.files[key] = f
	return []int{int(f.Fd())}, []byte("abcdef"), nil
}

func (s *sampleFDSource) Recover(key string, data []byte) error {
	if s.stopped {
		return errors.New("sampleFDSource is stopped")
	}

	var fdData sampleFDData
	if err := json.Unmarshal(data, &fdData); err != nil {
		return fmt.Errorf("error unmarshalling json: %v", err)
	}

	if fdData.Content != "42" {
		return fmt.Errorf("bad data passed to Recover: %q", data)
	}

	if _, found := s.files[key]; found {
		return fmt.Errorf("key %q is already present", key)
	}

	s.files[key] = nil
	return nil
}

func (s *sampleFDSource) Release(key string) error {
	if s.stopped {
		return errors.New("sampleFDSource is stopped")
	}

	f, found := s.files[key]
	if !found {
		return fmt.Errorf("file not found: %q", key)
	}
	delete(s.files, key)

	// "recovered" entries don't have FDs
	if f != nil {
		if err := f.Close(); err != nil {
			return fmt.Errorf("can't close file %q: %v", f.Name(), err)
		}
	}

	return nil
}

func (s *sampleFDSource) GetInfo(key string) ([]byte, error) {
	if s.stopped {
		return nil, errors.New("sampleFDSource is stopped")
	}

	_, found := s.files[key]
	if !found {
		return nil, fmt.Errorf("file not found: %q", key)
	}
	return []byte("info_" + key), nil
}

func (s *sampleFDSource) Stop() error {
	s.stopped = true
	return nil
}

func (s *sampleFDSource) isEmpty() bool {
	return len(s.files) == 0
}

func (s *sampleFDSource) isRecovered(key string) bool {
	f, found := s.files[key]
	return found && f == nil
}

func verifyFD(t *testing.T, c *FDClient, key string, data string) {
	fds, info, err := c.GetFDs(key)
	if err != nil {
		t.Fatalf("GetFDs(): %v", err)
	}

	expectedInfo := "info_" + key
	if string(info) != expectedInfo {
		t.Errorf("bad info: %q instead of %q", info, expectedInfo)
	}

	f1 := os.NewFile(uintptr(fds[0]), "acquired-fd")
	defer f1.Close()

	content, err := ioutil.ReadAll(f1)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}

	if string(content) != data {
		t.Errorf("bad content: %q instead of %q", content, data)
	}
}

func withFDClient(t *testing.T, toCall func(*FDClient, *sampleFDSource)) {
	tmpDir, err := ioutil.TempDir("", "pass-fd-test")
	if err != nil {
		t.Fatalf("ioutil.TempDir(): %v", err)
	}
	defer os.RemoveAll(tmpDir)

	socketPath := filepath.Join(tmpDir, "passfd")
	src := newSampleFDSource(tmpDir)
	s := NewFDServer(socketPath, src)
	if err := s.Serve(); err != nil {
		t.Fatalf("Serve(): %v", err)
	}
	defer func() {
		s.Stop()
		if !src.stopped {
			t.Errorf("FDSource not stopped")
		}
	}()
	c := NewFDClient(socketPath)

	toCall(c, src)
}

func TestFDServer(t *testing.T) {
	withFDClient(t, func(c *FDClient, src *sampleFDSource) {
		content := []string{"foo", "bar", "baz"}
		for _, data := range content {
			var err error
			key := "k_" + data
			respData, err := c.AddFDs(key, sampleFDData{Content: data})
			if err != nil {
				t.Fatalf("AddFDs(): %v", err)
			}
			expectedRespData := "abcdef"
			if string(respData) != expectedRespData {
				t.Errorf("bad data returned from add: %q instead of %q", data, expectedRespData)
			}
		}

		for _, data := range content {
			key := "k_" + data
			verifyFD(t, c, key, data)
		}

		for _, data := range content {
			key := "k_" + data
			if err := c.ReleaseFDs(key); err != nil {
				t.Fatalf("ReleaseFD(): key %q: %v", key, err)
			}
		}

		// here we make sure that releasing FDs works and also that passing errors from the
		// server works, too
		expectedErrorMessage := fmt.Sprintf("server returned error: bad fd key: \"k_foo\"")
		if _, _, err := c.GetFDs("k_foo"); err == nil {
			t.Errorf("GetFDs didn't return an error for a released fd")
		} else if err.Error() != expectedErrorMessage {
			t.Errorf("Bad error message from GetFD: %q instead of %q", err.Error(), expectedErrorMessage)
		}

		if !src.isEmpty() {
			t.Errorf("fd source is not empty (but it should be)")
		}
	})
}

func TestFDServerRecovery(t *testing.T) {
	withFDClient(t, func(c *FDClient, src *sampleFDSource) {
		if err := c.Recover("foobar", sampleFDData{"42"}); err != nil {
			t.Errorf("Recover(): %v", err)
		}
		if !src.isRecovered("foobar") {
			t.Errorf("the key is not recovered")
		}
		if err := c.ReleaseFDs("foobar"); err != nil {
			t.Errorf("Error releasing the recovered FDs: %v", err)
		}
	})
}
