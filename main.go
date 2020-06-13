// Command sensu-sh is a shell that supports querying JSON data from Sensu
// events.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func main() {
	prog := &Prog{}
	os.Exit(prog.Main(context.Background(), os.Args[1:]))
}

type Prog struct {
	event map[string]interface{}

	defaultExec interp.ExecHandlerFunc
	defaultEnv  expand.Environ
	runner      *interp.Runner
}

func (p *Prog) Main(ctx context.Context, args []string) int {
	log.SetFlags(0)
	log.SetPrefix("sensu-sh: ")

	flags := flag.NewFlagSet("sensu-sh", flag.ContinueOnError)
	// -event FILE
	eventFile := "-"
	flags.StringVar(&eventFile, "E", eventFile, "The event file to expose to the script. (long: -event)")
	flags.StringVar(&eventFile, "event", eventFile, "The event file to expose to the script. (short: -e)")
	// -raw
	rawScript := false
	flags.BoolVar(&rawScript, "R", rawScript, "Whether to treat all subsequent arguments as command strings. (long: -raw)")
	flags.BoolVar(&rawScript, "raw", rawScript, "Whether to treat all subsequent arguments as command strings. (short: -r)")

	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 2
	} else if err != nil {
		log.Print(err)
		return 1
	}

	if rawScript && flags.NArg() == 0 {
		log.Printf("no commands given")
		return 1
	} else if !rawScript && flags.NArg() == 0 {
		log.Printf("no script file given")
		return 1
	}

	var (
		prog   string
		params interp.RunnerOption
	)

	if rawScript {
		srcArgs, parArgs := flags.Args(), []string(nil)
		for i, arg := range srcArgs {
			if arg == "--" {
				srcArgs, parArgs = srcArgs[:i], srcArgs[i+1:]
				break
			}
		}
		prog = "#!sensu-sh\n" + strings.Join(srcArgs, "\n")
		params = interp.Params(parArgs...)
	} else {
		prog = flags.Arg(0)
		params = interp.Params(flags.Args()[1:]...)
		if prog == "-" && eventFile == "-" {
			log.Printf("both --event and program and stdin: only one can be read from standard input")
			return 1
		}
	}

	p.defaultExec = interp.DefaultExecHandler(time.Second * 5)
	p.defaultEnv = expand.ListEnviron(os.Environ()...)
	var err error
	p.runner, err = interp.New(
		interp.StdIO(nullStream{}, os.Stdout, os.Stderr),
		interp.ExecHandler(p.exec),
		params,
	)
	if err != nil {
		log.Printf("error creating interpreter: %v", err)
		return 1
	}

	p.event, err = readEvent(eventFile)
	if err != nil {
		log.Printf("error reading event file: %v", err)
		return 1
	}

	script, err := readScript(prog)
	if err != nil {
		log.Printf("error reading script file: %v", err)
		return 1
	}

	if err := p.runner.Run(context.Background(), script); err != nil {
		log.Printf("script error: %v", err)
		return 1
	}

	return 0
}

func (p *Prog) exec(ctx context.Context, args []string) error {
	cmd := args[0]
	switch cmd {
	case "query":
		return p.filterJSON(ctx, nil, args)
	case "event":
		return p.filterEvent(ctx, args)
	default: // @VAR [opt] [query]
		name := args[0]
		if name == "@" || !strings.HasPrefix(args[0], "@") {
			break
		}

		name = strings.TrimPrefix(name, "@")
		h := interp.HandlerCtx(ctx)
		v := h.Env.Get(name)
		if v.Kind != expand.String && v.Kind != expand.Indexed {
			break
		}
		return p.filterJSON(ctx, &name, append([]string{"query"}, args[1:]...))
	}

	return p.defaultExec(ctx, args)
}

func (p *Prog) filterJSON(ctx context.Context, forceVar *string, args []string) error {

	h := interp.HandlerCtx(ctx)
	logger := log.New(h.Stderr, "query: ", 0)
	f := flag.NewFlagSet("query", flag.ContinueOnError)
	f.SetOutput(h.Stderr)

	rawInput := false
	// -R, -raw-input
	f.BoolVar(&rawInput, "R", rawInput, "Read raw input as a string. (long: -raw-input)")
	f.BoolVar(&rawInput, "raw-input", rawInput, "Read raw input as a string. (short: -R)")

	filter := &jsonFilter{logger: logger}
	filter.bind(f)

	if err := f.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return interp.NewExitStatus(2)
	} else if err != nil {
		logger.Print(err)
		return interp.NewExitStatus(1)
	}

	args = f.Args()
	if len(args) == 0 {
		args = []string{"."}
	}
	if forceVar != nil {
		args = append([]string(nil), args...)
		args = append(args, *forceVar)
	}

	queryStr := "."
	source := "-"
	switch len(args) {
	case 2:
		source = args[1]
		fallthrough
	case 1:
		queryStr = args[0]
	case 0:
	default:
		logger.Printf("too many argument to query: expected 0..2")
		return interp.NewExitStatus(1)
	}

	var r io.Reader = h.Stdin
	if source != "-" {
		str := ""
		v := h.Env.Get(source)
		switch v.Kind {
		case expand.String:
			str = v.Str
		case expand.Indexed:
			str = strings.Join(v.List, "\n")
		default:
		}
		r = strings.NewReader(str)
	}

	if rawInput {
		data, err := ioutil.ReadAll(r)
		if err != nil {
			logger.Printf("error reading input: %v", err)
			return interp.NewExitStatus(1)
		}
		return filter.run(ctx, queryStr, string(data))
	}

	dec := yaml.NewDecoder(r)
	for {
		var input interface{}
		if err := dec.Decode(&input); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			logger.Printf("error decoding input: %v", err)
			return interp.NewExitStatus(1)
		}
		if err := filter.run(ctx, queryStr, input); err != nil {
			return err
		}
	}
}

func (p *Prog) filterEvent(ctx context.Context, args []string) error {
	h := interp.HandlerCtx(ctx)
	logger := log.New(h.Stderr, "event: ", 0)
	f := flag.NewFlagSet("event", flag.ContinueOnError)
	f.SetOutput(h.Stderr)

	filter := &jsonFilter{logger: logger}
	filter.bind(f)

	if err := f.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return interp.NewExitStatus(2)
	} else if err != nil {
		logger.Print(err)
		return interp.NewExitStatus(1)
	}

	queryStr := "."
	if f.NArg() == 1 {
		queryStr = f.Arg(0)
	} else if f.NArg() > 1 {
		logger.Printf("too many arguments to event: expected 0..1")
		return interp.NewExitStatus(1)
	}

	return filter.run(ctx, queryStr, p.event)
}

var errIncomplete = errors.New("attempt to parse incomplete script")

func readScript(path string) (*syntax.File, error) {
	var f io.ReadCloser
	if strings.HasPrefix(path, "#!sensu-sh\n") {
		f = ioutil.NopCloser(strings.NewReader(path))
	} else {
		var err error
		f, err = openFile(path)
		if err != nil {
			return nil, fmt.Errorf("error opening script [%s]: %w", path, err)
		}
		defer f.Close()
	}

	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(f, path)
	if err == nil && parser.Incomplete() {
		err = errIncomplete
	}
	if err != nil {
		return nil, fmt.Errorf("error parsing script [%s]: %w", path, err)
	}
	return file, nil
}

func openFile(path string) (io.ReadCloser, error) {
	if path == "-" {
		return ioutil.NopCloser(os.Stdin), nil
	}
	return os.Open(path)
}

func readEvent(path string) (map[string]interface{}, error) {
	var event map[string]interface{}
	f, err := openFile(path)
	if err != nil {
		return nil, fmt.Errorf("error opening event [%s]: %w", path, err)
	}
	defer f.Close()

	data, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("error reading event [%s]: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("error parsing event [%s]: %w", path, err)
	}
	return event, nil
}

type Encoder interface {
	Encode(interface{}) error
}

// plainEncoder is an encoder that writes string values values as raw strings to
// its output. All other values are formatted in some way. In particular, maps
// and slices are always encoded as compact JSON.
type plainEncoder struct {
	w       io.Writer
	written bool
}

func newPlainEncoder(w io.Writer) *plainEncoder {
	return &plainEncoder{w: w}
}

func (p *plainEncoder) Encode(val interface{}) error {
	var str string

	if p.written {
		if _, err := io.WriteString(p.w, "\n"); err != nil {
			return err
		}
	}
	p.written = true

	switch val := val.(type) {
	case map[string]interface{}, []interface{}:
		p, err := json.Marshal(val)
		if err != nil {
			return err
		}
		str = string(p)
	case string:
		str = val
	case float64:
		str = strconv.FormatFloat(val, 'f', -1, 64)
	default:
		str = fmt.Sprint(val)
	}

	if _, err := io.WriteString(p.w, str); err != nil {
		return err
	}

	return nil
}

type jsonFilter struct {
	pretty bool
	json   bool
	yaml   bool

	logger *log.Logger
	runner *interp.Runner
}

// bind attaches jsonFilter's options to a FlagSet.
func (j *jsonFilter) bind(f *flag.FlagSet) {
	// -j, -json
	f.BoolVar(&j.json, "j", j.json, "Print output as JSON. (long: -json)")
	f.BoolVar(&j.json, "json", j.json, "Print output as JSON. (short: -j)")
	// -Y, -yaml
	f.BoolVar(&j.yaml, "Y", j.yaml, "Output YAML instead of JSON or text. (long: -yaml)")
	f.BoolVar(&j.yaml, "yaml", j.yaml, "Output YAML instead of JSON or text. (short: -Y)")
	// -p, -pretty
	f.BoolVar(&j.pretty, "p", j.pretty, "Pretty-print JSON. (long: -pretty)")
	f.BoolVar(&j.pretty, "pretty", j.pretty, "Pretty-print JSON. (short: -p)")
}

// encoder returns an encoder configured for use by the receiver.
func (j *jsonFilter) encoder(w io.Writer) Encoder {
	if j.json {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		if j.pretty {
			enc.SetIndent("", "  ")
		}
		return enc
	} else if j.yaml {
		return yaml.NewEncoder(w)
	}
	return newPlainEncoder(w)
}

func (j *jsonFilter) run(ctx context.Context, queryStr string, input interface{}) error {
	h := interp.HandlerCtx(ctx)

	query, err := gojq.Parse(queryStr)
	if err != nil {
		j.logger.Printf("unable to parse query: %v", err)
		return interp.NewExitStatus(1)
	}

	enc := j.encoder(h.Stdout)

	iter := query.Run(input)
	for i := 0; ; i++ {
		val, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := val.(error); ok {
			j.logger.Printf("query error: %v", err)
			return interp.NewExitStatus(1)
		}

		if err := enc.Encode(val); err != nil {
			j.logger.Printf("encoding error: %v", err)
			return interp.NewExitStatus(1)
		}
	}

	return nil
}

// nullStream is an io.Reader with no contents.
type nullStream struct{}

func (nullStream) Read([]byte) (int, error) { return 0, io.EOF }
