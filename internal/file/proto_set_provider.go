// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package file

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/uber/prototool/internal/settings"
	"go.uber.org/zap"
)

type protoSetProvider struct {
	logger         *zap.Logger
	develMode      bool
	configData     string
	walkTimeout    time.Duration
	configProvider settings.ConfigProvider
}

func newProtoSetProvider(options ...ProtoSetProviderOption) *protoSetProvider {
	protoSetProvider := &protoSetProvider{
		logger:      zap.NewNop(),
		walkTimeout: DefaultWalkTimeout,
	}
	for _, option := range options {
		option(protoSetProvider)
	}
	configProviderOptions := []settings.ConfigProviderOption{
		settings.ConfigProviderWithLogger(protoSetProvider.logger),
	}
	if protoSetProvider.develMode {
		configProviderOptions = append(
			configProviderOptions,
			settings.ConfigProviderWithDevelMode(),
		)
	}
	protoSetProvider.configProvider = settings.NewConfigProvider(configProviderOptions...)
	return protoSetProvider
}

func (c *protoSetProvider) GetForDir(workDirPath string, dirPath string) (*ProtoSet, error) {
	protoSets, err := c.getMultipleForDir(workDirPath, dirPath)
	if err != nil {
		return nil, err
	}
	switch len(protoSets) {
	case 0:
		return nil, fmt.Errorf("no proto files found for dirPath %q", dirPath)
	case 1:
		return protoSets[0], nil
	default:
		configDirPaths := make([]string, 0, len(protoSets))
		for _, protoSet := range protoSets {
			configDirPaths = append(configDirPaths, protoSet.Config.DirPath)
		}
		return nil, fmt.Errorf("expected exactly one configuration file for dirPath %q, but found multiple in directories: %v", dirPath, configDirPaths)
	}
}

func (c *protoSetProvider) getMultipleForDir(workDirPath string, dirPath string) ([]*ProtoSet, error) {
	workDirPath, err := AbsClean(workDirPath)
	if err != nil {
		return nil, err
	}
	absDirPath, err := AbsClean(dirPath)
	if err != nil {
		return nil, err
	}
	// If c.configData != ", the user has specified configuration via the command line.
	// Set the configuration directory to the current working directory.
	configDirPath := workDirPath
	if c.configData == "" {
		configFilePath, err := c.configProvider.GetFilePathForDir(absDirPath)
		if err != nil {
			return nil, err
		}
		// we need everything for generation, not just the files in the given directory
		// so we go back to the config file if it is shallower
		// display path will be unaffected as this is based on workDirPath
		configDirPath = absDirPath
		if configFilePath != "" {
			configDirPath = filepath.Dir(configFilePath)
		}
	}
	protoFiles, err := c.walkAndGetAllProtoFiles(workDirPath, configDirPath)
	if err != nil {
		return nil, err
	}
	dirPathToProtoFiles := getDirPathToProtoFiles(protoFiles)
	protoSets, err := c.getBaseProtoSets(workDirPath, dirPathToProtoFiles)
	if err != nil {
		return nil, err
	}
	for _, protoSet := range protoSets {
		protoSet.WorkDirPath = workDirPath
		protoSet.DirPath = absDirPath
	}
	c.logger.Debug("returning ProtoSets", zap.String("workDirPath", workDirPath), zap.String("dirPath", dirPath), zap.Any("protoSets", protoSets))
	return protoSets, nil
}

func (c *protoSetProvider) getBaseProtoSets(absWorkDirPath string, dirPathToProtoFiles map[string][]*ProtoFile) ([]*ProtoSet, error) {
	filePathToProtoSet := make(map[string]*ProtoSet)
	for dirPath, protoFiles := range dirPathToProtoFiles {
		var configFilePath string
		var err error
		// we only want one ProtoSet if we have set configData
		// since we are overriding all configuration files
		if c.configData == "" {
			configFilePath, err = c.configProvider.GetFilePathForDir(dirPath)
			if err != nil {
				return nil, err
			}
		}
		protoSet, ok := filePathToProtoSet[configFilePath]
		if !ok {
			protoSet = &ProtoSet{
				DirPathToFiles: make(map[string][]*ProtoFile),
			}
			filePathToProtoSet[configFilePath] = protoSet
		}
		protoSet.DirPathToFiles[dirPath] = append(protoSet.DirPathToFiles[dirPath], protoFiles...)
		var config settings.Config
		if c.configData != "" {
			config, err = c.configProvider.GetForData(absWorkDirPath, c.configData)
			if err != nil {
				return nil, err
			}
		} else if configFilePath != "" {
			// configFilePath is empty if no config file is found
			config, err = c.configProvider.Get(configFilePath)
			if err != nil {
				return nil, err
			}
		}
		protoSet.Config = config
	}
	protoSets := make([]*ProtoSet, 0, len(filePathToProtoSet))
	for _, protoSet := range filePathToProtoSet {
		protoSets = append(protoSets, protoSet)
	}
	sort.Slice(protoSets, func(i int, j int) bool {
		return protoSets[i].Config.DirPath < protoSets[j].Config.DirPath
	})
	return protoSets, nil
}

// walkAndGetAllProtoFiles collects the .proto files nested under the given absDirPath.
// absDirPath represents the absolute path at which the configuration file is
// found, whereas absWorkDirPath represents absolute path at which prototool was invoked.
// absWorkDirPath is only used to determine the ProtoFile.DisplayPath, also known as
// the relative path from where prototool was invoked.
func (c *protoSetProvider) walkAndGetAllProtoFiles(absWorkDirPath string, absDirPath string) ([]*ProtoFile, error) {
	var (
		protoFiles     []*ProtoFile
		numWalkedFiles int
		timedOut       bool
	)
	allExcludes := make(map[string]struct{})
	// if we have a configData, we compute the exclude prefixes once
	// from this dirPath and data, and do not do it again in the below walk function
	if c.configData != "" {
		excludes, err := c.configProvider.GetExcludePrefixesForData(absWorkDirPath, c.configData)
		if err != nil {
			return nil, err
		}
		for _, exclude := range excludes {
			allExcludes[exclude] = struct{}{}
		}
	}
	walkErrC := make(chan error)
	go func() {
		walkErrC <- filepath.Walk(
			absDirPath,
			func(filePath string, fileInfo os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				numWalkedFiles++
				if timedOut {
					return fmt.Errorf("walking the diectory structure looking for proto files "+
						"timed out after %v and having seen %d files, are you sure you are operating "+
						"in the right context?", c.walkTimeout, numWalkedFiles)
				}
				// Verify if we should skip this directory/file.
				if fileInfo.IsDir() {
					// Add the excluded files with respect to the current file path.
					// Do not add if we have configData.
					if c.configData == "" {
						excludes, err := c.configProvider.GetExcludePrefixesForDir(filePath)
						if err != nil {
							return err
						}
						for _, exclude := range excludes {
							allExcludes[exclude] = struct{}{}
						}
					}
					if isExcluded(filePath, absDirPath, allExcludes) {
						return filepath.SkipDir
					}
					return nil
				}
				if filepath.Ext(filePath) != ".proto" {
					return nil
				}
				if isExcluded(filePath, absDirPath, allExcludes) {
					return nil
				}

				// Visit this file.
				displayPath, err := filepath.Rel(absWorkDirPath, filePath)
				if err != nil {
					displayPath = filePath
				}
				displayPath = filepath.Clean(displayPath)
				protoFiles = append(protoFiles, &ProtoFile{
					Path:        filePath,
					DisplayPath: displayPath,
				})
				return nil
			},
		)
	}()
	if c.walkTimeout == 0 {
		if walkErr := <-walkErrC; walkErr != nil {
			return nil, walkErr
		}
		return protoFiles, nil
	}
	select {
	case walkErr := <-walkErrC:
		if walkErr != nil {
			return nil, walkErr
		}
		return protoFiles, nil
	case <-time.After(c.walkTimeout):
		timedOut = true
		if walkErr := <-walkErrC; walkErr != nil {
			return nil, walkErr
		}
		return nil, fmt.Errorf("internal prototool error")
	}
}

func getDirPathToProtoFiles(protoFiles []*ProtoFile) map[string][]*ProtoFile {
	dirPathToProtoFiles := make(map[string][]*ProtoFile)
	for _, protoFile := range protoFiles {
		dir := filepath.Dir(protoFile.Path)
		dirPathToProtoFiles[dir] = append(dirPathToProtoFiles[dir], protoFile)
	}
	return dirPathToProtoFiles
}

// isExcluded determines whether the given filePath should be excluded.
// Note that all excludes are assumed to be cleaned absolute paths at
// this point.
// stopPath represents the absolute path to the prototool configuration.
// This is used to determine when we should stop checking for excludes.
func isExcluded(filePath, stopPath string, excludes map[string]struct{}) bool {
	// Use the root as a fallback so that we don't loop forever.
	root := filepath.Dir(string(filepath.Separator))

	isNested := func(curr, exclude string) bool {
		for {
			if curr == stopPath || curr == root {
				return false
			}
			if curr == exclude {
				return true
			}
			curr = filepath.Dir(curr)
		}
	}
	for exclude := range excludes {
		if isNested(filePath, exclude) {
			return true
		}
	}
	return false

}
