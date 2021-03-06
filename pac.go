package main

import (
	"bytes"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/robertkrimen/otto"
)

const pacMaxStatements = 10

var (
	pacStatementSplit                    *regexp.Regexp
	pacItemSplit                         *regexp.Regexp
	pacCallFindProxyForURLResultCount    *expvar.Map
	pacCallFindProxyForURLParamHostCount *expvar.Map
)

func init() {
	pacStatementSplit = regexp.MustCompile(`\s*;\s*`)
	pacItemSplit = regexp.MustCompile(`\s+`)

	pacCallFindProxyForURLResultCount = new(expvar.Map).Init()
	pacCallFindProxyForURLParamHostCount = new(expvar.Map).Init()

	callFindProxyForURLMap := new(expvar.Map).Init()
	callFindProxyForURLMap.Set("resultCount", pacCallFindProxyForURLResultCount)
	callFindProxyForURLMap.Set("urlHostCount", pacCallFindProxyForURLParamHostCount)

	pacExpvarMap := expvar.NewMap("pac")
	pacExpvarMap.Set("callFindProxyForURL", callFindProxyForURLMap)

}

// Pac is the main proxy auto configuration engine.
type Pac struct {
	mutex       *sync.RWMutex
	runtime     *gopacRuntime
	pacFile     string
	pacSrc      []byte
	ConnService *PacConnService
}

// NewPac create a new pac instance.
func NewPac() (*Pac, error) {
	p := &Pac{
		mutex:       &sync.RWMutex{},
		ConnService: NewPacConnService(),
	}
	if err := p.Unload(); err != nil {
		return nil, err
	}
	return p, nil
}

// Unload any previously loaded pac configuration and reverts to default.
func (p *Pac) Unload() error {
	return p.Load(MustAsset("default.pac"))
}

// Load attempts to load a pac from a string, a byte slice,
// a bytes.Buffer, or an io.Reader, but it MUST always be in UTF-8.
func (p *Pac) Load(js interface{}) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.pacFile = ""
	return p.initPacRuntime(js)
}

// LoadFile attempt to load a pac file.
func (p *Pac) LoadFile(file string) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	p.pacFile, _ = filepath.Abs(f.Name())
	return p.initPacRuntime(f)
}

func (p *Pac) initPacRuntime(js interface{}) error {
	var err error
	p.ConnService.Clear()
	p.runtime, err = newGopacRuntime()
	if err != nil {
		return err
	}
	formatForConsole := func(argumentList []otto.Value) string {
		output := []string{}
		for _, argument := range argumentList {
			output = append(output, fmt.Sprintf("%v", argument))
		}
		return strings.Join(output, " ")
	}
	p.runtime.vm.Set("alert", func(call otto.FunctionCall) otto.Value {
		log.Println("alert:", formatForConsole(call.ArgumentList))
		return otto.UndefinedValue()
	})
	p.runtime.vm.Set("console", map[string]interface{}{
		"assert": func(call otto.FunctionCall) otto.Value {
			if b, _ := call.Argument(0).ToBoolean(); !b {
				log.Println("console.assert:", formatForConsole(call.ArgumentList[1:]))
			}
			return otto.UndefinedValue()
		},
		"clear": func(call otto.FunctionCall) otto.Value {
			log.Println("console.clear: -------------------------------------")
			return otto.UndefinedValue()
		},
		"debug": func(call otto.FunctionCall) otto.Value {
			log.Println("console.debug:", formatForConsole(call.ArgumentList))
			return otto.UndefinedValue()
		},
		"error": func(call otto.FunctionCall) otto.Value {
			log.Println("console.error:", formatForConsole(call.ArgumentList))
			return otto.UndefinedValue()
		},
		"info": func(call otto.FunctionCall) otto.Value {
			log.Println("console.info:", formatForConsole(call.ArgumentList))
			return otto.UndefinedValue()
		},
		"log": func(call otto.FunctionCall) otto.Value {
			log.Println("console.log:", formatForConsole(call.ArgumentList))
			return otto.UndefinedValue()
		},
		"warn": func(call otto.FunctionCall) otto.Value {
			log.Println("console.warn:", formatForConsole(call.ArgumentList))
			return otto.UndefinedValue()
		},
	})
	switch js := js.(type) {
	case string:
		p.pacSrc = []byte(js)
	case []byte:
		p.pacSrc = js
	case *bytes.Buffer:
		p.pacSrc = js.Bytes()
	case io.Reader:
		var buf bytes.Buffer
		io.Copy(&buf, js)
		p.pacSrc = buf.Bytes()
	default:
		return errors.New("invalid source")
	}
	if _, err := p.runtime.vm.Run(p.pacSrc); err != nil {
		return err
	}
	return nil
}

// PacFilename returns the path of the corrently loaded pac configuration.
// Returns an empty string is the pac configuration was not loaded from a file.
func (p *Pac) PacFilename() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.pacFile
}

// PacConfiguration will return the current pac configuration
func (p *Pac) PacConfiguration() []byte {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.pacSrc
}

// HostFromURL takes a URL and return the host as it would be passed
// to the FindProxtForURL host argument.
func (p *Pac) HostFromURL(in *url.URL) string {
	if o := strings.Index(in.Host, ":"); o >= 0 {
		return in.Host[:o]
	}
	return in.Host
}

// CallFindProxyForURLFromURL using the current pac for a *url.URL.
func (p *Pac) CallFindProxyForURLFromURL(in *url.URL) (string, error) {
	return p.CallFindProxyForURL(in.String(), p.HostFromURL(in))
}

// CallFindProxyForURL using the current pac.
func (p *Pac) CallFindProxyForURL(url, host string) (string, error) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	pacCallFindProxyForURLParamHostCount.Add(host, 1)
	s, e := p.runtime.findProxyForURL(url, host)
	if s != "" {
		pacCallFindProxyForURLResultCount.Add(s, 1)
	}
	return s, e
}

// PacConn returns a *PacConn for the in *url.URL, processing
// the result of the pac find proxy result and trying to ensure that
// the proxy is active.
func (p *Pac) PacConn(in *url.URL) (*PacConn, error) {
	if in == nil {
		return nil, nil
	}
	urlStr := in.String()
	hostStr := p.HostFromURL(in)
	s, err := p.CallFindProxyForURL(urlStr, hostStr)
	if err != nil {
		return nil, err
	}
	errMsg := bytes.NewBufferString(
		fmt.Sprintf(
			"Unable to process FindProxyForURL(%q, %q) result %q.",
			urlStr,
			hostStr,
			s,
		),
	)
	for _, statement := range pacStatementSplit.Split(s, pacMaxStatements) {
		part := pacItemSplit.Split(statement, 2)
		switch strings.ToUpper(part[0]) {
		case "DIRECT":
			return nil, nil
		case "PROXY":
			pacConn := p.ConnService.Conn(part[1])
			if pacConn.IsActive() {
				return pacConn, nil
			}
			errMsg.Write([]byte("\n"))
			errMsg.WriteString(pacConn.Error().Error())
			errMsg.Write([]byte("."))
		default:
			errMsg.Write([]byte("\n"))
			errMsg.WriteString(
				fmt.Sprintf("Unsupported PAC command %q.", part[0]),
			)
			return nil, errors.New(errMsg.String())
		}
	}
	return nil, errors.New(errMsg.String())
}

// Proxy returns the URL of the proxy that the client should use.
// If the client should establish a direct connect that it will return
// nil. Can be easly wrapped for use in http.Transport.Proxy
func (p *Pac) Proxy(in *url.URL) (*url.URL, error) {
	pc, err := p.PacConn(in)
	if pc != nil {
		return url.Parse("http://" + pc.Address())
	}
	return nil, err
}

// Dial can be used for http.Transport.Dial and allows us to reuse
// a net.Conn that we might already have to a proxy server.
func (p *Pac) Dial(n, address string) (net.Conn, error) {
	if p.ConnService.IsKnownProxy(address) {
		return p.ConnService.Conn(address).Dial()
	}
	return net.Dial(n, address)
}
