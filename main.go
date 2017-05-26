/*
 * docktar - create tar archives of binaries with all dynamic libraries
 *
 * Copyright (C) 2017 Georg Großberger <contact@grossberger-ge.org>

 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.

 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.

 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"archive/tar"
	"bytes"
	"debug/elf"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type dataFile struct {
	Path   string
	Target string
	Elf    bool
}

type libFile struct {
	Name string
	Path string
	File string
}

const (
	dockerfileTmpl = `FROM scratch

ADD %s /
`
)

var (
	libPaths = []string{
		"/lib/",
		"/lib64/",
		"/usr/lib/",
		"/usr/lib64/",
		"/usr/local/lib/",
		"/usr/local/lib64/",
		"/lib/x86_64-linux-gnu/",
		"/usr/lib/x86_64-linux-gnu",
		"/usr/local/lib/x86_64-linux-gnu",
	}
	deps       = make(map[string]*libFile, 0)
	strip      = flag.Bool("s", false, "Strip binaries of debug symbols. Requires strip to be installed")
	dockerfile = flag.Bool("d", false, "Write Dockerfile next to tar. Ignored when using stdout.")
	outfile    = flag.String("o", "docker.tar", "Write archive to given file. Use value '-' for stdout.")
)

func main() {
	defer func() {
		if err := recover(); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			flag.PrintDefaults()
			os.Exit(1)
		}
	}()

	flag.Parse()
	fileArgs := make([]dataFile, 0)

	for _, a := range flag.Args() {
		arg := strings.Split(a, ":")
		file := dataFile{Path: arg[0], Elf: false}

		switch len(arg) {
		case 1:
			file.Path = arg[0]
			file.Target = arg[0]
			break
		case 2:
			file.Path = arg[0]
			file.Target = arg[1]
			break
		default:
			yell("Invalid argument: " + a)
		}

		fileArgs = append(fileArgs, file)
	}

	for i := len(fileArgs) - 1; i >= 0; i-- {
		file := fileArgs[i]
		if strings.Contains(file.Path, "*") {
			files, err := filepath.Glob(file.Path)
			if err != nil {
				yell("%s is not a valid glob pattern: %s", file.Path, err)
			}

			baseDir := ""
			if file.Path != file.Target {
				baseDir = file.Target
			}

			for j, fileName := range files {
				newFile := dataFile{Path: fileName, Target: fileName}
				if baseDir != "" {
					newFile.Target = filepath.Join(baseDir, filepath.Base(newFile.Path))
				}

				if j > 0 {
					fileArgs = append(fileArgs, newFile)
				} else {
					fileArgs[i] = newFile
				}
			}
		}
	}

	files := make([]dataFile, 0)

	for _, file := range fileArgs {
		if !isFile(file.Path) {
			newPath, err := exec.LookPath(file.Path)
			if err != nil {
				yell("Cannot find file %s: %s", file.Path, err)
			}

			if !strings.HasPrefix(newPath, "/") {
				newPath, err = filepath.Abs(newPath)
				if err != nil {
					yell("Cannot resolve absolute path of %s: %s", newPath, err)
				}
			}

			if file.Target == file.Path {
				file.Target = newPath
			}
			file.Path = newPath
		}

		if !strings.Contains(file.Path, "/") {
			d, err := os.Getwd()
			if err != nil {
				yell("Source %s is not an absolute file path, but cannot resolve current working directory: %s", file.Path, err)
			}
			file.Path = filepath.Join(d, file.Path)
			if !strings.Contains(file.Target, "/") {
				file.Target = file.Path
			}
		}

		stat, err := os.Stat(file.Path)
		if err != nil {
			yell("File %s does not exist", file.Path)
		}

		for stat.Mode()&os.ModeSymlink > 0 {
			newPath, err := filepath.EvalSymlinks(file.Path)
			if err != nil {
				yell("Cannot resolve symlink %s: %s", file.Path, err)
			}

			file.Path = newPath
			stat, err = os.Stat(newPath)
			if err != nil {
				yell("Cannot stat file %s: %s", file.Path, err)
			}
		}

		if !stat.Mode().IsRegular() {
			yell("File %s is not a regular file", file.Path)
		}

		if e, err := elf.Open(file.Path); err == nil && e != nil {
			if l, err := e.ImportedLibraries(); err == nil && len(l) > 0 {
				file.Elf = true
			}
		}

		files = append(files, file)
	}

	if len(files) < 1 {
		yell("Not enough arguments")
	}

	sched := make([]string, 0)

	for _, f := range files {
		if f.Elf {
			sched = append(sched, f.Path)
		}
	}

	resolveAll(sched)

	buf := new(bytes.Buffer)
	arc := tar.NewWriter(buf)

	for _, f := range files {
		addFile(arc, f.Path, f.Target, f.Elf)
	}

	for _, d := range deps {
		addFile(arc, d.File, d.Path, true)
	}

	arc.Close()

	if *outfile == "-" {
		_, err := io.Copy(os.Stdout, buf)
		if err != nil {
			yell("Cannot write to stdout: %s", err)
		}
	} else {
		f, err := os.Create(*outfile)
		if err != nil {
			yell("Cannot create archive %s: %s", *outfile, err)
		}
		defer f.Close()

		_, err = io.Copy(f, buf)
		if err != nil {
			yell("Cannot write to archive %s: %s", f.Name(), err)
		}

		if *dockerfile {
			outFilepath, _ := filepath.Abs(f.Name())
			outFilename := filepath.Base(outFilepath)
			dockerfileCnt := fmt.Sprintf(dockerfileTmpl, outFilename)
			ioutil.WriteFile(filepath.Join(filepath.Dir(outFilepath), "Dockerfile"), []byte(dockerfileCnt), 0644)
		}
	}
}

func isFile(name string) bool {
	d, err := os.Stat(name)
	if err != nil {
		return false
	}
	if m := d.Mode(); !m.IsDir() {
		return true
	}
	return false
}

func addFile(archive *tar.Writer, name, as string, isElf bool) {
	s, err := os.Stat(name)
	if err != nil {
		yell("Cannot stat file %s: %s", name, err)
	}

	h, err := tar.FileInfoHeader(s, "")
	if err != nil {
		yell("Cannot create tar file header for %s: %s", name, err)
	}

	data := readFile(name, isElf)
	h.Name = trSlash(as)
	h.Size = int64(len(data))

	err = archive.WriteHeader(h)
	if err != nil {
		yell("Cannot write file header: %s", err)
	}

	_, err = archive.Write(data)
	if err != nil {
		yell("Cannot write file data: %s", err)
	}
}

func readFile(name string, isElf bool) []byte {
	if *strip && isElf {
		tmpfile, err := ioutil.TempFile("", "docktar-stripped")
		if err != nil {
			yell("Cannot create tmp file: %s", err)
		}
		tmp := tmpfile.Name()
		tmpfile.Close()
		defer os.Remove(tmp)

		cmd := exec.Command("strip", "--strip-all", "-o", tmp, name)
		err = cmd.Run()
		if err != nil {
			yell("Cannot strip file: %s", err)
		}

		name = tmp
	}

	data, err := ioutil.ReadFile(name)
	if err != nil {
		yell("Cannot read file %s: %s", name, err)
	}

	return data
}

func resolveAll(bins []string) {
	for _, b := range bins {
		if _, ok := deps[b]; !ok {
			data, err := elf.Open(b)
			if err != nil {
				yell("Cannot open %s: %s", b, err)
			}

			libs, err := data.ImportedLibraries()
			if err != nil {
				yell("Cannot read elf imports of %s: %s\n", b, err)
			}

			subBins := make([]string, 0)

			for _, i := range libs {
				libdata, err := resolveLib(i)

				if err != nil {
					yell("Cannot resolve lib %s: %s", i, err)
				}

				deps[i] = libdata
				subBins = append(subBins, libdata.File)
			}

			resolveAll(subBins)
		}
	}
}

func resolveLib(name string) (*libFile, error) {
	for _, p := range libPaths {
		imported := filepath.Join(p, name)
		actual, _ := filepath.EvalSymlinks(imported)

		stat, err := os.Stat(actual)
		if err != nil {
			continue
		}

		if stat != nil {
			return &libFile{Name: name, Path: imported, File: actual}, nil
		}
	}

	return nil, errors.New("Did not find library " + name)
}

func trSlash(s string) string {
	for strings.HasPrefix(s, "/") {
		s = strings.TrimLeft(s, "/")
	}
	return s
}

func yell(format string, a ...interface{}) {
	panic(fmt.Sprintf(format+"\n", a...))
}
