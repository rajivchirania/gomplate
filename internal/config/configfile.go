package config

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

var (
	// PluginTimeoutKey - context key for PluginTimeout - temporary!
	PluginTimeoutKey = struct{}{}
)

// Parse a config file
func Parse(in io.Reader) (*Config, error) {
	out := &Config{}
	dec := yaml.NewDecoder(in)
	err := dec.Decode(out)
	if err != nil && err != io.EOF {
		return out, err
	}
	return out, nil
}

// Config -
type Config struct {
	Input       string   `yaml:"in,omitempty"`
	InputFiles  []string `yaml:"inputFiles,omitempty,flow"`
	InputDir    string   `yaml:"inputDir,omitempty"`
	ExcludeGlob []string `yaml:"excludes,omitempty"`
	OutputFiles []string `yaml:"outputFiles,omitempty,flow"`
	OutputDir   string   `yaml:"outputDir,omitempty"`
	OutputMap   string   `yaml:"outputMap,omitempty"`

	SuppressEmpty bool     `yaml:"suppressEmpty,omitempty"`
	ExecPipe      bool     `yaml:"execPipe,omitempty"`
	PostExec      []string `yaml:"postExec,omitempty,flow"`

	OutMode       string            `yaml:"chmod,omitempty"`
	LDelim        string            `yaml:"leftDelim,omitempty"`
	RDelim        string            `yaml:"rightDelim,omitempty"`
	DataSources   DSources          `yaml:"datasources,omitempty"`
	Context       DSources          `yaml:"context,omitempty"`
	Plugins       map[string]string `yaml:"plugins,omitempty"`
	PluginTimeout time.Duration     `yaml:"pluginTimeout,omitempty"`
	Templates     []string          `yaml:"templates,omitempty"`

	// Extra HTTP headers not attached to pre-defined datsources. Potentially
	// used by datasources defined in the template.
	ExtraHeaders map[string]http.Header `yaml:"-"`

	// internal use only, can't be injected in YAML
	PostExecInput io.ReadWriter `yaml:"-"`
	OutWriter     io.Writer     `yaml:"-"`
}

// DSources - map of datasource configs
type DSources map[string]DSConfig

func (d DSources) mergeFrom(o DSources) DSources {
	for k, v := range o {
		c, ok := d[k]
		if ok {
			d[k] = c.mergeFrom(v)
		} else {
			d[k] = v
		}
	}
	return d
}

// DSConfig - datasource config
type DSConfig struct {
	URL    *url.URL    `yaml:"-"`
	Header http.Header `yaml:"header,omitempty,flow"`
}

// UnmarshalYAML - satisfy the yaml.Umarshaler interface - URLs aren't
// well supported, and anyway we need to do some extra parsing
func (d *DSConfig) UnmarshalYAML(value *yaml.Node) error {
	type raw struct {
		URL    string
		Header http.Header
	}
	r := raw{}
	err := value.Decode(&r)
	if err != nil {
		return err
	}
	u, err := parseSourceURL(r.URL)
	if err != nil {
		return fmt.Errorf("could not parse datasource URL %q: %w", r.URL, err)
	}
	*d = DSConfig{
		URL:    u,
		Header: r.Header,
	}
	return nil
}

// MarshalYAML - satisfy the yaml.Marshaler interface - URLs aren't
// well supported, and anyway we need to do some extra parsing
func (d DSConfig) MarshalYAML() (interface{}, error) {
	type raw struct {
		URL    string
		Header http.Header
	}
	r := raw{
		URL:    d.URL.String(),
		Header: d.Header,
	}
	return r, nil
}

func (d DSConfig) mergeFrom(o DSConfig) DSConfig {
	if o.URL != nil {
		d.URL = o.URL
	}
	if d.Header == nil {
		d.Header = o.Header
	} else {
		for k, v := range o.Header {
			d.Header[k] = v
		}
	}
	return d
}

// MergeFrom - use this Config as the defaults, and override it with any
// non-zero values from the other Config
//
// Note that Input/InputDir/InputFiles will override each other, as well as
// OutputDir/OutputFiles.
func (c *Config) MergeFrom(o *Config) *Config {
	switch {
	case !isZero(o.Input):
		c.Input = o.Input
		c.InputDir = ""
		c.InputFiles = nil
		c.OutputDir = ""
	case !isZero(o.InputDir):
		c.Input = ""
		c.InputDir = o.InputDir
		c.InputFiles = nil
	case !isZero(o.InputFiles):
		if !(len(o.InputFiles) == 1 && o.InputFiles[0] == "-") {
			c.Input = ""
			c.InputFiles = o.InputFiles
			c.InputDir = ""
			c.OutputDir = ""
		}
	}

	if !isZero(o.OutputMap) {
		c.OutputDir = ""
		c.OutputFiles = nil
		c.OutputMap = o.OutputMap
	}
	if !isZero(o.OutputDir) {
		c.OutputDir = o.OutputDir
		c.OutputFiles = nil
		c.OutputMap = ""
	}
	if !isZero(o.OutputFiles) {
		c.OutputDir = ""
		c.OutputFiles = o.OutputFiles
		c.OutputMap = ""
	}
	if !isZero(o.ExecPipe) {
		c.ExecPipe = o.ExecPipe
		c.PostExec = o.PostExec
		c.OutputFiles = o.OutputFiles
	}
	if !isZero(o.ExcludeGlob) {
		c.ExcludeGlob = o.ExcludeGlob
	}
	if !isZero(o.OutMode) {
		c.OutMode = o.OutMode
	}
	if !isZero(o.LDelim) {
		c.LDelim = o.LDelim
	}
	if !isZero(o.RDelim) {
		c.RDelim = o.RDelim
	}
	if !isZero(o.Templates) {
		c.Templates = o.Templates
	}
	c.DataSources.mergeFrom(o.DataSources)
	c.Context.mergeFrom(o.Context)
	if len(o.Plugins) > 0 {
		for k, v := range o.Plugins {
			c.Plugins[k] = v
		}
	}

	return c
}

// ParseDataSourceFlags - sets the DataSources and Context fields from the
// key=value format flags as provided at the command-line
func (c *Config) ParseDataSourceFlags(datasources, contexts, headers []string) error {
	for _, d := range datasources {
		k, ds, err := parseDatasourceArg(d)
		if err != nil {
			return err
		}
		if c.DataSources == nil {
			c.DataSources = DSources{}
		}
		c.DataSources[k] = ds
	}
	for _, d := range contexts {
		k, ds, err := parseDatasourceArg(d)
		if err != nil {
			return err
		}
		if c.Context == nil {
			c.Context = DSources{}
		}
		c.Context[k] = ds
	}

	hdrs, err := parseHeaderArgs(headers)
	if err != nil {
		return err
	}

	for k, v := range hdrs {
		if d, ok := c.Context[k]; ok {
			d.Header = v
			c.Context[k] = d
			delete(hdrs, k)
		}
		if d, ok := c.DataSources[k]; ok {
			d.Header = v
			c.DataSources[k] = d
			delete(hdrs, k)
		}
	}
	if len(hdrs) > 0 {
		c.ExtraHeaders = hdrs
	}
	return nil
}

// ParsePluginFlags - sets the Plugins field from the
// key=value format flags as provided at the command-line
func (c *Config) ParsePluginFlags(plugins []string) error {
	for _, plugin := range plugins {
		parts := strings.SplitN(plugin, "=", 2)
		if len(parts) < 2 {
			return fmt.Errorf("plugin requires both name and path")
		}
		if c.Plugins == nil {
			c.Plugins = map[string]string{}
		}
		c.Plugins[parts[0]] = parts[1]
	}
	return nil
}

func parseDatasourceArg(value string) (key string, ds DSConfig, err error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) == 1 {
		f := parts[0]
		key = strings.SplitN(value, ".", 2)[0]
		if path.Base(f) != f {
			err = fmt.Errorf("invalid datasource (%s): must provide an alias with files not in working directory", value)
			return key, ds, err
		}
		ds.URL, err = absFileURL(f)
	} else if len(parts) == 2 {
		key = parts[0]
		ds.URL, err = parseSourceURL(parts[1])
	}
	return key, ds, err
}

func parseHeaderArgs(headerArgs []string) (map[string]http.Header, error) {
	headers := make(map[string]http.Header)
	for _, v := range headerArgs {
		ds, name, value, err := splitHeaderArg(v)
		if err != nil {
			return nil, err
		}
		if _, ok := headers[ds]; !ok {
			headers[ds] = make(http.Header)
		}
		headers[ds][name] = append(headers[ds][name], strings.TrimSpace(value))
	}
	return headers, nil
}

func splitHeaderArg(arg string) (datasourceAlias, name, value string, err error) {
	parts := strings.SplitN(arg, "=", 2)
	if len(parts) != 2 {
		err = fmt.Errorf("invalid datasource-header option '%s'", arg)
		return "", "", "", err
	}
	datasourceAlias = parts[0]
	name, value, err = splitHeader(parts[1])
	return datasourceAlias, name, value, err
}

func splitHeader(header string) (name, value string, err error) {
	parts := strings.SplitN(header, ":", 2)
	if len(parts) != 2 {
		err = fmt.Errorf("invalid HTTP Header format '%s'", header)
		return "", "", err
	}
	name = http.CanonicalHeaderKey(parts[0])
	value = parts[1]
	return name, value, nil
}

// Validate the Config
func (c Config) Validate() (err error) {
	err = notTogether(
		[]string{"in", "inputFiles", "inputDir"},
		c.Input, c.InputFiles, c.InputDir)
	if err == nil {
		err = notTogether(
			[]string{"outputFiles", "outputDir", "outputMap"},
			c.OutputFiles, c.OutputDir, c.OutputMap)
	}
	if err == nil {
		err = notTogether(
			[]string{"outputDir", "outputMap", "execPipe"},
			c.OutputDir, c.OutputMap, c.ExecPipe)
	}

	if err == nil {
		err = mustTogether("outputDir", "inputDir",
			c.OutputDir, c.InputDir)
	}

	if err == nil {
		err = mustTogether("outputMap", "inputDir",
			c.OutputMap, c.InputDir)
	}

	if err == nil {
		f := len(c.InputFiles)
		if f == 0 && c.Input != "" {
			f = 1
		}
		o := len(c.OutputFiles)
		if f != o && !c.ExecPipe {
			err = fmt.Errorf("must provide same number of 'outputFiles' (%d) as 'in' or 'inputFiles' (%d) options", o, f)
		}
	}

	if err == nil {
		if c.ExecPipe && len(c.PostExec) == 0 {
			err = fmt.Errorf("execPipe may only be used with a postExec command")
		}
	}

	if err == nil {
		if c.ExecPipe && (len(c.OutputFiles) > 0 && c.OutputFiles[0] != "-") {
			err = fmt.Errorf("must not set 'outputFiles' when using 'execPipe'")
		}
	}

	return err
}

func notTogether(names []string, values ...interface{}) error {
	found := ""
	for i, value := range values {
		if isZero(value) {
			continue
		}
		if found != "" {
			return fmt.Errorf("only one of these options is supported at a time: '%s', '%s'",
				found, names[i])
		}
		found = names[i]
	}
	return nil
}

func mustTogether(left, right string, lValue, rValue interface{}) error {
	if !isZero(lValue) && isZero(rValue) {
		return fmt.Errorf("these options must be set together: '%s', '%s'",
			left, right)
	}

	return nil
}

func isZero(value interface{}) bool {
	switch v := value.(type) {
	case string:
		return v == ""
	case []string:
		return len(v) == 0
	case bool:
		return !v
	default:
		return false
	}
}

// ApplyDefaults -
func (c *Config) ApplyDefaults() {
	if c.InputDir != "" && c.OutputDir == "" && c.OutputMap == "" {
		c.OutputDir = "."
	}
	if c.Input == "" && c.InputDir == "" && len(c.InputFiles) == 0 {
		c.InputFiles = []string{"-"}
	}
	if c.OutputDir == "" && c.OutputMap == "" && len(c.OutputFiles) == 0 && !c.ExecPipe {
		c.OutputFiles = []string{"-"}
	}
	if c.LDelim == "" {
		c.LDelim = "{{"
	}
	if c.RDelim == "" {
		c.RDelim = "}}"
	}

	if c.ExecPipe {
		c.PostExecInput = &bytes.Buffer{}
		c.OutWriter = c.PostExecInput
		c.OutputFiles = []string{"-"}
	} else {
		c.PostExecInput = os.Stdin
		c.OutWriter = os.Stdout
	}

	if c.PluginTimeout == 0 {
		c.PluginTimeout = 5 * time.Second
	}
}

// String -
func (c *Config) String() string {
	out := &strings.Builder{}
	out.WriteString("---\n")
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)

	// dereferenced copy so we can truncate input for display
	c2 := *c
	if len(c2.Input) >= 11 {
		c2.Input = c2.Input[0:8] + "..."
	}

	err := enc.Encode(c2)
	if err != nil {
		return err.Error()
	}
	return out.String()
}

func parseSourceURL(value string) (*url.URL, error) {
	if value == "-" {
		value = "stdin://"
	}
	value = filepath.ToSlash(value)
	// handle absolute Windows paths
	volName := ""
	if volName = filepath.VolumeName(value); volName != "" {
		// handle UNCs
		if len(volName) > 2 {
			value = "file:" + value
		} else {
			value = "file:///" + value
		}
	}
	srcURL, err := url.Parse(value)
	if err != nil {
		return nil, err
	}

	if volName != "" && len(srcURL.Path) >= 3 {
		if srcURL.Path[0] == '/' && srcURL.Path[2] == ':' {
			srcURL.Path = srcURL.Path[1:]
		}
	}

	if !srcURL.IsAbs() {
		srcURL, err = absFileURL(value)
		if err != nil {
			return nil, err
		}
	}
	return srcURL, nil
}

func absFileURL(value string) (*url.URL, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrapf(err, "can't get working directory")
	}
	wd = filepath.ToSlash(wd)
	baseURL := &url.URL{
		Scheme: "file",
		Path:   wd + "/",
	}
	relURL, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("can't parse value %s as URL: %w", value, err)
	}
	resolved := baseURL.ResolveReference(relURL)
	// deal with Windows drive letters
	if !strings.HasPrefix(wd, "/") && resolved.Path[2] == ':' {
		resolved.Path = resolved.Path[1:]
	}
	return resolved, nil
}

// GetMode - parse an os.FileMode out of the string, and let us know if it's an override or not...
func (c *Config) GetMode() (os.FileMode, bool, error) {
	modeOverride := c.OutMode != ""
	m, err := strconv.ParseUint("0"+c.OutMode, 8, 32)
	if err != nil {
		return 0, false, err
	}
	mode := os.FileMode(m)
	if mode == 0 && c.Input != "" {
		mode = 0644
	}
	return mode, modeOverride, nil
}
