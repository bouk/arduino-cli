// This file is part of arduino-cli.
//
// Copyright 2020 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU General Public License version 3,
// which covers the main part of arduino-cli.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/gpl-3.0.en.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package sketch

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/arduino/arduino-cli/arduino/globals"
	"github.com/arduino/arduino-cli/i18n"
	"github.com/arduino/go-paths-helper"
	"github.com/pkg/errors"
)

// Sketch holds all the files composing a sketch
type Sketch struct {
	Name             string
	MainFile         *paths.Path
	FullPath         *paths.Path // FullPath is the path to the Sketch folder
	BuildPath        *paths.Path
	OtherSketchFiles paths.PathList // Sketch files that end in .ino other than main file
	AdditionalFiles  paths.PathList
	RootFolderFiles  paths.PathList // All files that are in the Sketch root
	Project          *Project
}

var tr = i18n.Tr

// New creates an Sketch instance by reading all the files composing a sketch and grouping them
// by file type.
func New(path *paths.Path) (*Sketch, error) {
	if path == nil {
		return nil, fmt.Errorf(tr("sketch path is not valid"))
	}

	path = path.Canonical()
	if exist, err := path.ExistCheck(); err != nil {
		return nil, fmt.Errorf("%s: %s", tr("sketch path is not valid"), err)
	} else if !exist {
		return nil, fmt.Errorf("%s: %s", tr("no such file or directory"), path)
	}
	if _, validIno := globals.MainFileValidExtensions[path.Ext()]; validIno && !path.IsDir() {
		path = path.Parent()
	}

	var mainFile *paths.Path
	for ext := range globals.MainFileValidExtensions {
		candidateSketchMainFile := path.Join(path.Base() + ext)
		if candidateSketchMainFile.Exist() {
			if mainFile == nil {
				mainFile = candidateSketchMainFile
			} else {
				return nil, errors.Errorf(tr("multiple main sketch files found (%[1]v, %[2]v)"),
					mainFile,
					candidateSketchMainFile,
				)
			}
		}
	}
	if mainFile == nil {
		return nil, fmt.Errorf(tr("main file missing from sketch: %s", path.Join(path.Base()+globals.MainFileValidExtension)))
	}

	sketch := &Sketch{
		Name:             path.Base(),
		MainFile:         mainFile,
		FullPath:         path,
		BuildPath:        GenBuildPath(path),
		OtherSketchFiles: paths.PathList{},
		AdditionalFiles:  paths.PathList{},
		RootFolderFiles:  paths.PathList{},
		Project:          &Project{},
	}

	if projectFile := sketch.GetProjectPath(); projectFile.Exist() {
		prj, err := LoadProjectFile(projectFile)
		if err != nil {
			return nil, fmt.Errorf("%s %w", tr("error loading sketch project file:"), err)
		}
		sketch.Project = prj
	}

	err := sketch.checkSketchCasing()
	if e, ok := err.(*InvalidSketchFolderNameError); ok {
		return nil, e
	}
	if err != nil {
		return nil, err
	}

	if mainFile == nil {
		return nil, fmt.Errorf(tr("can't find main Sketch file in %s"), path)
	}

	sketchFolderFiles, err := sketch.supportedFiles()
	if err != nil {
		return nil, err
	}

	// Collect files
	for _, p := range *sketchFolderFiles {
		// Skip files that can't be opened
		f, err := p.Open()
		if err != nil {
			continue
		}
		f.Close()

		ext := p.Ext()
		if _, found := globals.MainFileValidExtensions[ext]; found {
			if p.EqualsTo(mainFile) {
				// The main file must not be included in the lists of other files
				continue
			}
			// file is a valid sketch file, see if it's stored at the
			// sketch root and ignore if it's not.
			if p.Parent().EqualsTo(path) {
				sketch.OtherSketchFiles.Add(p)
				sketch.RootFolderFiles.Add(p)
			}
		} else if _, found := globals.AdditionalFileValidExtensions[ext]; found {
			// If the user exported the compiles binaries to the Sketch "build" folder
			// they would be picked up but we don't want them, so we skip them like so
			if isInBuildFolder, err := p.IsInsideDir(sketch.FullPath.Join("build")); isInBuildFolder || err != nil {
				continue
			}

			sketch.AdditionalFiles.Add(p)
			if p.Parent().EqualsTo(path) {
				sketch.RootFolderFiles.Add(p)
			}
		} else {
			return nil, errors.Errorf(tr("unknown sketch file extension '%s'"), ext)
		}
	}

	sort.Sort(&sketch.AdditionalFiles)
	sort.Sort(&sketch.OtherSketchFiles)
	sort.Sort(&sketch.RootFolderFiles)

	return sketch, nil
}

// supportedFiles reads all files recursively contained in Sketch and
// filter out unneded or unsupported ones and returns them
func (s *Sketch) supportedFiles() (*paths.PathList, error) {
	files, err := s.FullPath.ReadDirRecursive()
	if err != nil {
		return nil, err
	}
	files.FilterOutDirs()
	files.FilterOutHiddenFiles()
	validExtensions := []string{}
	for ext := range globals.MainFileValidExtensions {
		validExtensions = append(validExtensions, ext)
	}
	for ext := range globals.AdditionalFileValidExtensions {
		validExtensions = append(validExtensions, ext)
	}
	files.FilterSuffix(validExtensions...)
	return &files, nil

}

// GetProfile returns the requested profile or nil if the profile
// is not found.
func (s *Sketch) GetProfile(profileName string) *Profile {
	for _, p := range s.Project.Profiles {
		if p.Name == profileName {
			return p
		}
	}
	return nil
}

// checkSketchCasing returns an error if the casing of the sketch folder and the main file are different.
// Correct:
//
//	MySketch/MySketch.ino
//
// Wrong:
//
//	MySketch/mysketch.ino
//	mysketch/MySketch.ino
//
// This is mostly necessary to avoid errors on Mac OS X.
// For more info see: https://github.com/arduino/arduino-cli/issues/1174
func (s *Sketch) checkSketchCasing() error {
	files, err := s.FullPath.ReadDir()
	if err != nil {
		return errors.Errorf(tr("reading files: %v"), err)
	}
	files.FilterOutDirs()

	candidateFileNames := []string{}
	for ext := range globals.MainFileValidExtensions {
		candidateFileNames = append(candidateFileNames, fmt.Sprintf("%s%s", s.Name, ext))
	}
	files.FilterPrefix(candidateFileNames...)

	if files.Len() == 0 {
		sketchFile := s.FullPath.Join(s.Name + globals.MainFileValidExtension)
		return &InvalidSketchFolderNameError{
			SketchFolder: s.FullPath,
			SketchFile:   sketchFile,
		}
	}

	return nil
}

// GetProjectPath returns the path to the sketch project file (sketch.yaml or sketch.yml)
func (s *Sketch) GetProjectPath() *paths.Path {
	projectFile := s.FullPath.Join("sketch.yaml")
	if !projectFile.Exist() {
		alternateProjectFile := s.FullPath.Join("sketch.yml")
		if alternateProjectFile.Exist() {
			return alternateProjectFile
		}
	}
	return projectFile
}

// GetDefaultFQBN returns the default FQBN for the sketch (from the sketch.yaml project file), or the
// empty string if not set.
func (s *Sketch) GetDefaultFQBN() string {
	return s.Project.DefaultFqbn
}

// GetDefaultPortAddressAndProtocol returns the default port address and port protocol for the sketch
// (from the sketch.yaml project file), or empty strings if not set.
func (s *Sketch) GetDefaultPortAddressAndProtocol() (string, string) {
	return s.Project.DefaultPort, s.Project.DefaultProtocol
}

// SetDefaultFQBN sets the default FQBN for the sketch and saves it in the sketch.yaml project file.
func (s *Sketch) SetDefaultFQBN(fqbn string) error {
	s.Project.DefaultFqbn = fqbn
	return updateOrAddYamlRootEntry(s.GetProjectPath(), "default_fqbn", fqbn)
}

// SetDefaultPort sets the default port address and port protocol for the sketch and saves it in the
// sketch.yaml project file.
func (s *Sketch) SetDefaultPort(address, protocol string) error {
	s.Project.DefaultPort = address
	s.Project.DefaultProtocol = protocol
	if err := updateOrAddYamlRootEntry(s.GetProjectPath(), "default_port", address); err != nil {
		return err
	}
	return updateOrAddYamlRootEntry(s.GetProjectPath(), "default_protocol", protocol)
}

// InvalidSketchFolderNameError is returned when the sketch directory doesn't match the sketch name
type InvalidSketchFolderNameError struct {
	SketchFolder *paths.Path
	SketchFile   *paths.Path
}

func (e *InvalidSketchFolderNameError) Error() string {
	return tr("no valid sketch found in %[1]s: missing %[2]s", e.SketchFolder, e.SketchFile)
}

// CheckForPdeFiles returns all files ending with .pde extension
// in sketch, this is mainly used to warn the user that these files
// must be changed to .ino extension.
// When .pde files won't be supported anymore this function must be removed.
func CheckForPdeFiles(sketch *paths.Path) []*paths.Path {
	if sketch.IsNotDir() {
		sketch = sketch.Parent()
	}

	files, err := sketch.ReadDirRecursive()
	if err != nil {
		return []*paths.Path{}
	}
	files.FilterSuffix(".pde")
	return files
}

// GenBuildPath generates a suitable name for the build folder.
// The sketchPath, if not nil, is also used to furhter differentiate build paths.
func GenBuildPath(sketchPath *paths.Path) *paths.Path {
	path := ""
	if sketchPath != nil {
		path = sketchPath.String()
	}
	md5SumBytes := md5.Sum([]byte(path))
	md5Sum := strings.ToUpper(hex.EncodeToString(md5SumBytes[:]))
	return paths.TempDir().Join("arduino-sketch-" + md5Sum)
}
