package newrelic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/newrelic/go-agent/v3/internal"
	"github.com/newrelic/go-agent/v3/internal/logger"
	"github.com/newrelic/go-agent/v3/internal/sysinfo"
	"github.com/newrelic/go-agent/v3/internal/utilization"
)

func copyDestConfig(c AttributeDestinationConfig) AttributeDestinationConfig {
	cp := c
	if nil != c.Include {
		cp.Include = make([]string, len(c.Include))
		copy(cp.Include, c.Include)
	}
	if nil != c.Exclude {
		cp.Exclude = make([]string, len(c.Exclude))
		copy(cp.Exclude, c.Exclude)
	}
	return cp
}

func copyConfigReferenceFields(cfg Config) Config {
	cp := cfg
	if nil != cfg.Labels {
		cp.Labels = make(map[string]string, len(cfg.Labels))
		for key, val := range cfg.Labels {
			cp.Labels[key] = val
		}
	}
	if nil != cfg.ErrorCollector.IgnoreStatusCodes {
		ignored := make([]int, len(cfg.ErrorCollector.IgnoreStatusCodes))
		copy(ignored, cfg.ErrorCollector.IgnoreStatusCodes)
		cp.ErrorCollector.IgnoreStatusCodes = ignored
	}

	cp.Attributes = copyDestConfig(cfg.Attributes)
	cp.ErrorCollector.Attributes = copyDestConfig(cfg.ErrorCollector.Attributes)
	cp.TransactionEvents.Attributes = copyDestConfig(cfg.TransactionEvents.Attributes)
	cp.TransactionTracer.Attributes = copyDestConfig(cfg.TransactionTracer.Attributes)
	cp.BrowserMonitoring.Attributes = copyDestConfig(cfg.BrowserMonitoring.Attributes)
	cp.SpanEvents.Attributes = copyDestConfig(cfg.SpanEvents.Attributes)
	cp.TransactionTracer.Segments.Attributes = copyDestConfig(cfg.TransactionTracer.Segments.Attributes)

	return cp
}

func transportSetting(t http.RoundTripper) interface{} {
	if nil == t {
		return nil
	}
	return fmt.Sprintf("%T", t)
}

func loggerSetting(lg Logger) interface{} {
	if nil == lg {
		return nil
	}
	if _, ok := lg.(logger.ShimLogger); ok {
		return nil
	}
	return fmt.Sprintf("%T", lg)
}

const (
	// https://source.datanerd.us/agents/agent-specs/blob/master/Custom-Host-Names.md
	hostByteLimit = 255
)

type settings Config

func (s settings) MarshalJSON() ([]byte, error) {
	c := Config(s)
	transport := c.Transport
	c.Transport = nil
	l := c.Logger
	c.Logger = nil

	js, err := json.Marshal(c)
	if nil != err {
		return nil, err
	}
	fields := make(map[string]interface{})
	err = json.Unmarshal(js, &fields)
	if nil != err {
		return nil, err
	}
	// The License field is not simply ignored by adding the `json:"-"` tag
	// to it since we want to allow consumers to populate Config from JSON.
	delete(fields, `License`)
	fields[`Transport`] = transportSetting(transport)
	fields[`Logger`] = loggerSetting(l)

	// Browser monitoring support.
	if c.BrowserMonitoring.Enabled {
		fields[`browser_monitoring.loader`] = "rum"
	}

	return json.Marshal(fields)
}

// labels is used for connect JSON formatting.
type labels map[string]string

func (l labels) MarshalJSON() ([]byte, error) {
	ls := make([]struct {
		Key   string `json:"label_type"`
		Value string `json:"label_value"`
	}, len(l))

	i := 0
	for key, val := range l {
		ls[i].Key = key
		ls[i].Value = val
		i++
	}

	return json.Marshal(ls)
}

func configConnectJSONInternal(c Config, pid int, util *utilization.Data, e environment, version string, securityPolicies *internal.SecurityPolicies, metadata map[string]string) ([]byte, error) {
	return json.Marshal([]interface{}{struct {
		Pid              int                         `json:"pid"`
		Language         string                      `json:"language"`
		Version          string                      `json:"agent_version"`
		Host             string                      `json:"host"`
		HostDisplayName  string                      `json:"display_host,omitempty"`
		Settings         interface{}                 `json:"settings"`
		AppName          []string                    `json:"app_name"`
		HighSecurity     bool                        `json:"high_security"`
		Labels           labels                      `json:"labels,omitempty"`
		Environment      environment                 `json:"environment"`
		Identifier       string                      `json:"identifier"`
		Util             *utilization.Data           `json:"utilization"`
		SecurityPolicies *internal.SecurityPolicies  `json:"security_policies,omitempty"`
		Metadata         map[string]string           `json:"metadata"`
		EventData        internal.EventHarvestConfig `json:"event_harvest_config"`
	}{
		Pid:             pid,
		Language:        internal.AgentLanguage,
		Version:         version,
		Host:            internal.StringLengthByteLimit(util.Hostname, hostByteLimit),
		HostDisplayName: internal.StringLengthByteLimit(c.HostDisplayName, hostByteLimit),
		Settings:        (settings)(c),
		AppName:         strings.Split(c.AppName, ";"),
		HighSecurity:    c.HighSecurity,
		Labels:          c.Labels,
		Environment:     e,
		// This identifier field is provided to avoid:
		// https://newrelic.atlassian.net/browse/DSCORE-778
		//
		// This identifier is used by the collector to look up the real
		// agent. If an identifier isn't provided, the collector will
		// create its own based on the first appname, which prevents a
		// single daemon from connecting "a;b" and "a;c" at the same
		// time.
		//
		// Providing the identifier below works around this issue and
		// allows users more flexibility in using application rollups.
		Identifier:       c.AppName,
		Util:             util,
		SecurityPolicies: securityPolicies,
		Metadata:         metadata,
		EventData:        internal.DefaultEventHarvestConfig(c.maxTxnEvents()),
	}})
}

const (
	// https://source.datanerd.us/agents/agent-specs/blob/master/Connect-LEGACY.md#metadata-hash
	metadataPrefix = "NEW_RELIC_METADATA_"
)

func gatherMetadata(env []string) map[string]string {
	metadata := make(map[string]string)
	for _, pair := range env {
		if strings.HasPrefix(pair, metadataPrefix) {
			idx := strings.Index(pair, "=")
			if idx >= 0 {
				metadata[pair[0:idx]] = pair[idx+1:]
			}
		}
	}
	return metadata
}

// config exists to avoid adding private fields to Config.
type config struct {
	Config
	metadata map[string]string
	hostname string
}

func (c Config) computeDynoHostname(getenv func(string) string) string {
	if !c.Heroku.UseDynoNames {
		return ""
	}
	dyno := getenv("DYNO")
	if dyno == "" {
		return ""
	}
	for _, prefix := range c.Heroku.DynoNamePrefixesToShorten {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(dyno, prefix+".") {
			dyno = prefix + ".*"
			break
		}
	}
	return dyno
}

func newInternalConfig(cfg Config, getenv func(string) string, environ []string) (config, error) {
	// Copy maps and slices to prevent race conditions if a consumer changes
	// them after calling NewApplication.
	cfg = copyConfigReferenceFields(cfg)
	if err := cfg.validate(); nil != err {
		return config{}, err
	}
	// Ensure that Logger is always set to avoid nil checks.
	if nil == cfg.Logger {
		cfg.Logger = logger.ShimLogger{}
	}
	var hostname string
	if host := cfg.computeDynoHostname(getenv); host != "" {
		hostname = host
	} else if host, err := sysinfo.Hostname(); err == nil {
		hostname = host
	} else {
		hostname = "unknown"
	}
	return config{
		Config:   cfg,
		metadata: gatherMetadata(environ),
		hostname: hostname,
	}, nil
}

func (c config) createConnectJSON(securityPolicies *internal.SecurityPolicies) ([]byte, error) {
	env := newEnvironment()
	util := utilization.Gather(utilization.Config{
		DetectAWS:         c.Utilization.DetectAWS,
		DetectAzure:       c.Utilization.DetectAzure,
		DetectPCF:         c.Utilization.DetectPCF,
		DetectGCP:         c.Utilization.DetectGCP,
		DetectDocker:      c.Utilization.DetectDocker,
		DetectKubernetes:  c.Utilization.DetectKubernetes,
		LogicalProcessors: c.Utilization.LogicalProcessors,
		TotalRAMMIB:       c.Utilization.TotalRAMMIB,
		BillingHostname:   c.Utilization.BillingHostname,
		Hostname:          c.hostname,
	}, c.Logger)
	return configConnectJSONInternal(c.Config, os.Getpid(), util, env, Version, securityPolicies, c.metadata)
}

var (
	preconnectHostDefault        = "collector.newrelic.com"
	preconnectRegionLicenseRegex = regexp.MustCompile(`(^.+?)x`)
)

func (c config) preconnectHost() string {
	if "" != c.Host {
		return c.Host
	}
	m := preconnectRegionLicenseRegex.FindStringSubmatch(c.License)
	if len(m) > 1 {
		return "collector." + m[1] + ".nr-data.net"
	}
	return preconnectHostDefault
}
