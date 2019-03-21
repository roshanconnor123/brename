// Copyright © 2017 Wei Shen <shenwei356@gmail.com>
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

package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/fatih/color"
	"github.com/mattn/go-colorable"
	"github.com/shenwei356/breader"
	"github.com/shenwei356/go-logging"
	"github.com/shenwei356/util/pathutil"
	"github.com/spf13/cobra"
)

var log *logging.Logger

var version = "2.8.1"
var app = "brename"

// for detecting one case where two or more files are renamed to same new path
var pathTree map[string]struct{}

// Options is the struct containing all global options
type Options struct {
	Verbose int
	Version bool
	DryRun  bool

	Pattern      string
	PatternRe    *regexp.Regexp
	Replacement  string
	Recursive    bool
	IncludingDir bool
	OnlyDir      bool
	MaxDepth     int
	IgnoreCase   bool
	IgnoreExt    bool

	IncludeFilters   []string
	ExcludeFilters   []string
	IncludeFilterRes []*regexp.Regexp
	ExcludeFilterRes []*regexp.Regexp

	ListPath    bool
	ListAbsPath bool

	ReplaceWithNR bool
	StartNum      int
	NRFormat      string

	ReplaceWithKV bool
	KVs           map[string]string
	KVFile        string
	KeepKey       bool
	KeyCaptIdx    int
	KeyMissRepl   string

	OverwriteMode int

	Undo             bool
	ForceUndo        bool
	LastOpDetailFile string
}

var reNR = regexp.MustCompile(`\{(NR|nr)\}`)
var reKV = regexp.MustCompile(`\{(KV|kv)\}`)

func getOptions(cmd *cobra.Command) *Options {
	undo := getFlagBool(cmd, "undo")
	forceUndo := getFlagBool(cmd, "force-undo")
	if undo || forceUndo {
		return &Options{
			Undo:             true, // set it true even only force-undo given
			ForceUndo:        forceUndo,
			LastOpDetailFile: ".brename_detail.txt",
		}
	}

	version := getFlagBool(cmd, "version")
	if version {
		checkVersion()
		return &Options{Version: version}
	}

	pattern := getFlagString(cmd, "pattern")
	if pattern == "" {
		log.Errorf("flag -p/--pattern needed")
		os.Exit(1)
	}
	p := pattern
	ignoreCase := getFlagBool(cmd, "ignore-case")
	if ignoreCase {
		p = "(?i)" + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		log.Errorf("illegal regular expression for search pattern: %s", pattern)
		os.Exit(1)
	}

	infilters := getFlagStringSlice(cmd, "include-filters")
	infilterRes := make([]*regexp.Regexp, 0, 10)
	var infilterRe *regexp.Regexp
	for _, infilter := range infilters {
		if infilter == "" {
			log.Errorf("value of flag -f/--include-filters missing")
			os.Exit(1)
		}
		infilterRe, err = regexp.Compile("(?i)" + infilter)
		if err != nil {
			log.Errorf("illegal regular expression for include filter: %s", infilter)
			os.Exit(1)
		}
		infilterRes = append(infilterRes, infilterRe)
	}

	exfilters := getFlagStringSlice(cmd, "exclude-filters")
	exfilterRes := make([]*regexp.Regexp, 0, 10)
	var exfilterRe *regexp.Regexp
	for _, exfilter := range exfilters {
		if exfilter == "" {
			log.Errorf("value of flag -F/--exclude-filters missing")
			os.Exit(1)
		}
		exfilterRe, err = regexp.Compile("(?i)" + exfilter)
		if err != nil {
			log.Errorf("illegal regular expression for exclude filter: %s", exfilter)
			os.Exit(1)
		}
		exfilterRes = append(exfilterRes, exfilterRe)
	}

	replacement := getFlagString(cmd, "replacement")
	kvFile := getFlagString(cmd, "kv-file")

	if kvFile != "" {
		if len(replacement) == 0 {
			checkError(fmt.Errorf("flag -r/--replacement needed when given flag -k/--kv-file"))
		}
		if !reKV.MatchString(replacement) {
			checkError(fmt.Errorf(`replacement symbol "{kv}"/"{KV}" not found in value of flag -r/--replacement when flag -k/--kv-file given`))
		}
	}

	var replaceWithNR bool
	if reNR.MatchString(replacement) {
		replaceWithNR = true
	}

	var replaceWithKV bool
	var kvs map[string]string
	keepKey := getFlagBool(cmd, "keep-key")
	keyMissRepl := getFlagString(cmd, "key-miss-repl")
	if reKV.MatchString(replacement) {
		replaceWithKV = true
		if !regexp.MustCompile(`\(.+\)`).MatchString(pattern) {
			checkError(fmt.Errorf(`value of -p/--pattern must contains "(" and ")" to capture data which is used specify the KEY`))
		}
		if kvFile == "" {
			checkError(fmt.Errorf(`since replacement symbol "{kv}"/"{KV}" found in value of flag -r/--replacement, tab-delimited key-value file should be given by flag -k/--kv-file`))
		}

		if keepKey && keyMissRepl != "" {
			log.Warning("flag -m/--key-miss-repl ignored when flag -K/--keep-key given")
		}
		log.Infof("read key-value file: %s", kvFile)
		kvs, err = readKVs(kvFile, ignoreCase)
		if err != nil {
			checkError(fmt.Errorf("read key-value file: %s", err))
		}
		if len(kvs) == 0 {
			checkError(fmt.Errorf("no valid data in key-value file: %s", kvFile))
		}

		log.Infof("%d pairs of key-value loaded", len(kvs))
	}

	verbose := getFlagNonNegativeInt(cmd, "verbose")
	if verbose > 2 {
		log.Errorf("illegal value of flag --verbose: %d, only 0/1/2 allowed", verbose)
		os.Exit(1)
	}

	overwriteMode := getFlagNonNegativeInt(cmd, "overwrite-mode")
	if overwriteMode > 2 {
		log.Errorf("illegal value of flag -o/--overwrite-mode: %d, only 0/1/2 allowed", overwriteMode)
		os.Exit(1)
	}

	return &Options{
		Verbose: verbose,
		Version: version,
		DryRun:  getFlagBool(cmd, "dry-run"),

		Pattern:      pattern,
		PatternRe:    re,
		Replacement:  replacement,
		Recursive:    getFlagBool(cmd, "recursive"),
		IncludingDir: getFlagBool(cmd, "including-dir"),
		OnlyDir:      getFlagBool(cmd, "only-dir"),
		MaxDepth:     getFlagNonNegativeInt(cmd, "max-depth"),
		IgnoreCase:   ignoreCase,
		IgnoreExt:    getFlagBool(cmd, "ignore-ext"),

		IncludeFilters:   infilters,
		IncludeFilterRes: infilterRes,
		ExcludeFilters:   infilters,
		ExcludeFilterRes: exfilterRes,

		ListPath:    getFlagBool(cmd, "list"),
		ListAbsPath: getFlagBool(cmd, "list-abs"),

		ReplaceWithNR: replaceWithNR,
		StartNum:      getFlagNonNegativeInt(cmd, "start-num"),
		NRFormat:      fmt.Sprintf("%%0%dd", getFlagPositiveInt(cmd, "nr-width")),
		ReplaceWithKV: replaceWithKV,

		KVs:         kvs,
		KVFile:      kvFile,
		KeepKey:     keepKey,
		KeyCaptIdx:  getFlagPositiveInt(cmd, "key-capt-idx"),
		KeyMissRepl: keyMissRepl,

		OverwriteMode: overwriteMode,

		Undo:             false,
		LastOpDetailFile: ".brename_detail.txt",
	}
}

func init() {
	logFormat := logging.MustStringFormatter(`%{color}[%{level:.4s}]%{color:reset} %{message}`)
	var stderr io.Writer = os.Stderr
	if runtime.GOOS == "windows" {
		stderr = colorable.NewColorableStderr()
	}
	backend := logging.NewLogBackend(stderr, "", 0)
	backendFormatter := logging.NewBackendFormatter(backend, logFormat)
	logging.SetBackend(backendFormatter)
	log = logging.MustGetLogger(app)

	RootCmd.Flags().IntP("verbose", "v", 0, "verbose level (0 for all, 1 for warning and error, 2 for only error) (default 0)")
	RootCmd.Flags().BoolP("version", "V", false, "print version information and check for update")
	RootCmd.Flags().BoolP("dry-run", "d", false, "print rename operations but do not run")

	RootCmd.Flags().StringP("pattern", "p", "", "search pattern (regular expression)")
	RootCmd.Flags().StringP("replacement", "r", "", `replacement. capture variables supported.  e.g. $1 represents the first submatch. ATTENTION: for *nix OS, use SINGLE quote NOT double quotes or use the \ escape character. Ascending integer is also supported by "{nr}"`)
	RootCmd.Flags().BoolP("recursive", "R", false, "rename recursively")
	RootCmd.Flags().BoolP("including-dir", "D", false, "rename directories")
	RootCmd.Flags().BoolP("only-dir", "", false, "only rename directories")
	RootCmd.Flags().IntP("max-depth", "", 0, "maximum depth for recursive search (0 for no limit)")
	RootCmd.Flags().BoolP("ignore-case", "i", false, "ignore case")
	RootCmd.Flags().BoolP("ignore-ext", "e", false, "ignore file extension. i.e., replacement does not change file extension")

	RootCmd.Flags().StringSliceP("include-filters", "f", []string{"."}, `include file filter(s) (regular expression, case ignored). multiple values supported, e.g., -f ".html" -f ".htm", but ATTENTION: comma in filter is treated as separater of multiple filters`)
	RootCmd.Flags().StringSliceP("exclude-filters", "F", []string{}, `exclude file filter(s) (regular expression, case ignored). multiple values supported, e.g., -F ".html" -F ".htm", but ATTENTION: comma in filter is treated as separater of multiple filters`)

	RootCmd.Flags().BoolP("list", "l", false, `only list paths that match pattern`)
	RootCmd.Flags().BoolP("list-abs", "a", false, `list absolute path, using along with -l/--list`)

	RootCmd.Flags().StringP("kv-file", "k", "",
		`tab-delimited key-value file for replacing key with value when using "{kv}" in -r (--replacement)`)
	RootCmd.Flags().BoolP("keep-key", "K", false, "keep the key as value when no value found for the key")
	RootCmd.Flags().IntP("key-capt-idx", "I", 1, "capture variable index of key (1-based)")
	RootCmd.Flags().StringP("key-miss-repl", "m", "", "replacement for key with no corresponding value")
	RootCmd.Flags().IntP("start-num", "n", 1, `starting number when using {nr} in replacement`)
	RootCmd.Flags().IntP("nr-width", "", 1, `minimum width for {nr} in flag -r/--replacement. e.g., formating "1" to "001" by --nr-width 3`)

	RootCmd.Flags().IntP("overwrite-mode", "o", 0, "overwrite mode (0 for reporting error, 1 for overwrite, 2 for not renaming) (default 0)")

	RootCmd.Flags().BoolP("undo", "u", false, "undo the LAST successful operation")
	RootCmd.Flags().BoolP("force-undo", "U", false, "continue undo even when some operation failed")

	RootCmd.Example = `  1. dry run and showing potential dangerous operations
      brename -p "abc" -d
  2. dry run and only show operations that will cause error
      brename -p "abc" -d -v 2
  3. only renaming specific paths via include filters
      brename -p ":" -r "-" -f ".htm$" -f ".html$"
  4. renaming all .jpeg files to .jpg in all subdirectories
      brename -p "\.jpeg" -r ".jpg" -R   dir
  5. using capture variables, e.g., $1, $2 ...
      brename -p "(a)" -r "\$1\$1"
      or brename -p "(a)" -r '$1$1' in Linux/Mac OS X
  6. renaming directory too
      brename -p ":" -r "-" -R -D   pdf-dirs
  7. using key-value file
      brename -p "(.+)" -r "{kv}" -k kv.tsv
  8. do not touch file extension
      brename -p ".+" -r "{nr}" -f .mkv -f .mp4 -e
  9. only list paths that match pattern (-l)
      brename -i -f '.docx?$' -p . -R -l
  10. undo the LAST successful operation
      brename -u

  More examples: https://github.com/shenwei356/brename`

	RootCmd.SetUsageTemplate(`Usage:{{if .Runnable}}
  {{if .HasAvailableFlags}}{{appendIfNotPresent .UseLine "[flags]"}}{{else}}{{.UseLine}}{{end}}{{end}}{{if .HasAvailableSubCommands}}
  {{ .CommandPath}} [command]{{end}} {{if gt .Aliases 0}}

Aliases:
  {{.NameAndAliases}}
{{end}}{{if .HasExample}}

Examples:
{{ .Example }}{{end}}{{ if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{ if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimRightSpace}}{{end}}{{ if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimRightSpace}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsHelpCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{ if .HasAvailableSubCommands }}

Use "{{.CommandPath}} --help" for more information about a command.{{end}}
`)

	pathTree = make(map[string]struct{}, 1000)
}

func main() {
	if err := RootCmd.Execute(); err != nil {
		log.Error(err)
		os.Exit(1)
	}
}

func checkError(err error) {
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}
}

func getFileList(args []string) []string {
	files := []string{}
	if len(args) == 0 {
		files = append(files, "./")
	} else {
		for _, file := range files {
			if file == "./" {
				continue
			}
			if _, err := os.Stat(file); os.IsNotExist(err) {
				checkError(err)
			}
		}
		files = args
	}
	return files
}

func getFlagBool(cmd *cobra.Command, flag string) bool {
	value, err := cmd.Flags().GetBool(flag)
	checkError(err)
	return value
}

func getFlagString(cmd *cobra.Command, flag string) string {
	value, err := cmd.Flags().GetString(flag)
	checkError(err)
	return value
}

func getFlagStringSlice(cmd *cobra.Command, flag string) []string {
	value, err := cmd.Flags().GetStringSlice(flag)
	checkError(err)
	return value
}

func getFlagPositiveInt(cmd *cobra.Command, flag string) int {
	value, err := cmd.Flags().GetInt(flag)
	checkError(err)
	if value <= 0 {
		checkError(fmt.Errorf("value of flag --%s should be greater than 0", flag))
	}
	return value
}

func getFlagNonNegativeInt(cmd *cobra.Command, flag string) int {
	value, err := cmd.Flags().GetInt(flag)
	checkError(err)
	if value < 0 {
		checkError(fmt.Errorf("value of flag --%s should be greater than or equal to 0", flag))
	}
	return value
}

func checkVersion() {
	fmt.Printf("%s v%s\n", app, version)
	fmt.Println("\nChecking new version...")

	resp, err := http.Get(fmt.Sprintf("https://github.com/shenwei356/%s/releases/latest", app))
	if err != nil {
		checkError(fmt.Errorf("Network error"))
	}
	items := strings.Split(resp.Request.URL.String(), "/")
	var v string
	if items[len(items)-1] == "" {
		v = items[len(items)-2]
	} else {
		v = items[len(items)-1]
	}
	if v == "v"+version {
		fmt.Printf("You are using the latest version of %s\n", app)
	} else {
		fmt.Printf("New version available: %s %s at %s\n", app, v, resp.Request.URL.String())
	}
}

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   app,
	Short: "a cross-platform command-line tool for safely batch renaming files/directories via regular expression",
	Long: fmt.Sprintf(`
brename -- a practical cross-platform command-line tool for safely batch renaming files/directories via regular expression

Version: %s

Author: Wei Shen <shenwei356@gmail.com>

Homepage: https://github.com/shenwei356/brename

Attention:
  1. Paths starting with "." is ignored.
  2. Flag -f/--include-filters and -F/--exclude-filters support multiple values,
     e.g., -f ".html" -f ".htm".
     But ATTENTION: comma in filter is treated as separater of multiple filters.

Special replacement symbols:

  {nr}    Ascending integer
  {kv}    Corresponding value of the key (captured variable $n) by key-value file,
          n can be specified by flag -I/--key-capt-idx (default: 1)


`, version),
	Run: func(cmd *cobra.Command, args []string) {
		// var err error
		opt := getOptions(cmd)

		if opt.Version {
			return
		}

		var delimiter = "\t_shenwei356-brename_\t"
		if opt.Undo {
			existed, err := pathutil.Exists(opt.LastOpDetailFile)
			checkError(err)
			if !existed {
				log.Infof("no brename operation to undo")
				return
			}

			history := make([]operation, 0, 1000)

			fn := func(line string) (interface{}, bool, error) {
				line = strings.TrimRight(line, "\n")
				if line == "" || line[0] == '#' { // ignoring blank line and comment line
					return "", false, nil
				}
				items := strings.Split(line, delimiter)
				if len(items) != 2 {
					return items, false, nil
				}
				return operation{source: items[0], target: items[1], code: 0}, true, nil
			}

			var reader *breader.BufferedReader
			reader, err = breader.NewBufferedReader(opt.LastOpDetailFile, 2, 100, fn)
			checkError(err)

			var op operation
			for chunk := range reader.Ch {
				checkError(chunk.Err)
				for _, data := range chunk.Data {
					op = data.(operation)
					history = append(history, op)
				}
			}
			if len(history) == 0 {
				log.Infof("no brename operation to undo")
				return
			}

			n := 0
			for i := len(history) - 1; i >= 0; i-- {
				op = history[i]

				err = os.Rename(op.target, op.source)
				if err != nil {
					log.Errorf(`fail to rename: '%s' -> '%s': %s`, op.source, op.target, err)
					if !opt.ForceUndo {
						log.Infof("%d path(s) renamed", n)
						os.Exit(1)
					}
				}
				n++
				log.Infof("rename back: '%s' -> '%s'", op.target, op.source)
			}
			log.Infof("%d path(s) renamed", n)

			checkError(os.Remove(opt.LastOpDetailFile))
			return
		}

		ops := make([]operation, 0, 1000)
		opCH := make(chan operation, 100)
		done := make(chan int)

		var hasErr bool
		var n, nErr int
		var outPath string
		var err error

		go func() {
			for op := range opCH {
				if opt.ListPath {
					if opt.ListAbsPath {
						outPath, err = filepath.Abs(op.source)
						checkError(err)
					} else {
						outPath = op.source
					}
					fmt.Println(outPath)
					continue
				}
				if int(op.code) >= opt.Verbose {
					switch op.code {
					case codeOK:
						log.Infof("checking: %s\n", op)
					case codeUnchanged:
						log.Warningf("checking: %s\n", op)
					case codeExisted, codeOverwriteNewPath:
						switch opt.OverwriteMode {
						case 0: // report error
							log.Errorf("checking: %s\n", op)
						case 1: // overwrite
							log.Warningf("checking: %s (will be overwrited)\n", op)
						case 2: // no renaming
							log.Warningf("checking: %s (will NOT be overwrited)\n", op)
						}
					case codeMissingTarget:
						log.Errorf("checking: %s\n", op)
					}
				}

				switch op.code {
				case codeOK:
					ops = append(ops, op)
					n++
				case codeUnchanged:
				case codeExisted, codeOverwriteNewPath:
					switch opt.OverwriteMode {
					case 0: // report error
						hasErr = true
						nErr++
						continue
					case 1: // overwrite
						ops = append(ops, op)
						n++
					case 2: // no renaming

					}
				default:
					hasErr = true
					nErr++
					continue
				}
			}
			done <- 1
		}()

		for _, path := range getFileList(args) {
			err = walk(opt, opCH, path, 1)
			if err != nil {
				close(opCH)
				checkError(err)
			}
		}
		close(opCH)
		<-done

		if hasErr {
			log.Errorf("%d potential error(s) detected, please check", nErr)
			os.Exit(1)
		}

		if opt.ListPath {
			return
		}
		log.Infof("%d path(s) to be renamed", n)
		if n == 0 {
			return
		}

		if opt.DryRun {
			return
		}

		var fh *os.File
		fh, err = os.Create(opt.LastOpDetailFile)
		checkError(err)
		bfh := bufio.NewWriter(fh)
		defer func() {
			checkError(bfh.Flush())
			fh.Close()
		}()

		var n2 int
		var targetDir string
		var targetDirExisted bool
		for _, op := range ops {
			targetDir = filepath.Dir(op.target)
			targetDirExisted, err = pathutil.DirExists(targetDir)
			if err != nil {
				log.Errorf(`fail to rename: '%s' -> '%s'`, op.source, op.target)
				os.Exit(1)
			}
			if !targetDirExisted {
				os.MkdirAll(targetDir, 0755)
			}

			err = os.Rename(op.source, op.target)
			if err != nil {
				log.Errorf(`fail to rename: '%s' -> '%s': %s`, op.source, op.target, err)
				os.Exit(1)
			}
			log.Infof("renamed: '%s' -> '%s'", op.source, op.target)
			bfh.WriteString(fmt.Sprintf("%s%s%s\n", op.source, delimiter, op.target))
			n2++
		}

		log.Infof("%d path(s) renamed", n2)
	},
}

type code int

const (
	codeOK code = iota
	codeUnchanged
	codeExisted
	codeOverwriteNewPath
	codeMissingTarget
)

var yellow = color.New(color.FgYellow).SprintFunc()
var red = color.New(color.FgRed).SprintFunc()
var green = color.New(color.FgGreen).SprintFunc()

func (c code) String() string {
	switch c {
	case codeOK:
		return green("ok")
	case codeUnchanged:
		return yellow("unchanged")
	case codeExisted:
		return red("new path existed")
	case codeOverwriteNewPath:
		return red("overwriting newly renamed path")
	case codeMissingTarget:
		return red("missing target")
	}

	return "undefined code"
}

type operation struct {
	source string
	target string
	code   code
}

func (op operation) String() string {
	return fmt.Sprintf(`[ %s ] '%s' -> '%s'`, op.code, op.source, op.target)
}

func checkOperation(opt *Options, path string) (bool, operation) {
	dir, filename := filepath.Split(path)
	var ext string
	if opt.IgnoreExt {
		ext = filepath.Ext(path)
		filename = filename[0 : len(filename)-len(ext)]
	}

	if !opt.PatternRe.MatchString(filename) {
		return false, operation{}
	}

	r := opt.Replacement

	if opt.ReplaceWithNR {
		r = reNR.ReplaceAllString(r, fmt.Sprintf(opt.NRFormat, opt.StartNum))
		opt.StartNum++
	}

	if opt.ReplaceWithKV {
		founds := opt.PatternRe.FindAllStringSubmatch(filename, -1)
		if len(founds) > 0 {
			found := founds[0]
			if opt.KeyCaptIdx > len(found)-1 {
				checkError(fmt.Errorf("value of flag -I/--key-capt-idx overflows"))
			}
			k := found[opt.KeyCaptIdx]
			if opt.IgnoreCase {
				k = strings.ToLower(k)
			}
			if _, ok := opt.KVs[k]; ok {
				r = reKV.ReplaceAllString(r, opt.KVs[k])
			} else if opt.KeepKey {
				r = reKV.ReplaceAllString(r, found[opt.KeyCaptIdx])
			} else if opt.KeyMissRepl != "" {
				r = reKV.ReplaceAllString(r, opt.KeyMissRepl)
			} else {
				return false, operation{path, path, codeUnchanged}
			}
		}
	}

	filename2 := opt.PatternRe.ReplaceAllString(filename, r) + ext
	target := filepath.Join(dir, filename2)

	if filename2 == "" {
		return true, operation{path, target, codeMissingTarget}
	}

	if filename2 == filename+ext {
		return true, operation{path, target, codeUnchanged}
	}

	if _, err := os.Stat(target); err == nil {
		return true, operation{path, target, codeExisted}
	}

	if _, ok := pathTree[target]; ok {
		return true, operation{path, target, codeOverwriteNewPath}
	}
	pathTree[target] = struct{}{}

	return true, operation{path, target, codeOK}
}

func ignore(opt *Options, path string) bool {
	for _, re := range opt.ExcludeFilterRes {
		if re.MatchString(path) {
			return true
		}
	}
	for _, re := range opt.IncludeFilterRes {
		if re.MatchString(path) {
			return false
		}
	}
	return true
}

func walk(opt *Options, opCh chan<- operation, path string, depth int) error {
	if opt.MaxDepth > 0 && depth > opt.MaxDepth {
		return nil
	}
	_, err := ioutil.ReadFile(path)
	// it's a file
	if err == nil {
		if ok, op := checkOperation(opt, path); ok {
			opCh <- op
		}
		return nil
	}

	// it's a directory
	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Warning(err)
	}

	var filename string
	_files := make([]string, 0, len(files))
	_dirs := make([]string, 0, len(files))
	for _, file := range files {
		filename = file.Name()

		if filename[0] == '.' {
			continue
		}

		if file.IsDir() {
			_dirs = append(_dirs, filename)
		} else {
			_files = append(_files, filename)
		}
	}

	if !opt.OnlyDir {
		for _, filename := range _files {
			if ignore(opt, filename) {
				continue
			}
			fileFullPath := filepath.Join(path, filename)
			if ok, op := checkOperation(opt, fileFullPath); ok {
				opCh <- op
			}
		}
	}

	// sub directory
	for _, filename := range _dirs {
		if (opt.OnlyDir || opt.IncludingDir) && ignore(opt, filename) {
			continue
		}

		fileFullPath := filepath.Join(path, filename)
		if opt.Recursive {
			err := walk(opt, opCh, fileFullPath, depth+1)
			if err != nil {
				return err
			}
		}
		// rename directories
		if (opt.OnlyDir || opt.IncludingDir) && !ignore(opt, filename) {
			if ok, op := checkOperation(opt, fileFullPath); ok {
				opCh <- op
			}
		}
	}

	return nil
}

func readKVs(file string, ignoreCase bool) (map[string]string, error) {
	type KV [2]string
	fn := func(line string) (interface{}, bool, error) {
		line = strings.TrimRight(line, "\r\n")
		if len(line) == 0 {
			return nil, false, nil
		}
		items := strings.Split(line, "\t")
		if len(items) < 2 {
			return nil, false, nil
		}
		if ignoreCase {
			return KV([2]string{strings.ToLower(items[0]), items[1]}), true, nil
		}
		return KV([2]string{items[0], items[1]}), true, nil
	}
	kvs := make(map[string]string)
	reader, err := breader.NewBufferedReader(file, 2, 10, fn)
	if err != nil {
		return kvs, err
	}
	var items KV
	for chunk := range reader.Ch {
		if chunk.Err != nil {
			return kvs, err
		}
		for _, data := range chunk.Data {
			items = data.(KV)
			kvs[items[0]] = items[1]
		}
	}
	return kvs, nil
}
