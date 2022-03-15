package domino

import (
	"bytes"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/psilva261/goja"
	"github.com/psilva261/goja/parser"
	"github.com/psilva261/goja_nodejs/console"
	"github.com/psilva261/goja_nodejs/eventloop"
	"github.com/psilva261/goja_nodejs/require"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	convert6to5 = os.Getenv("GOJAFS_6TO5")
	origin = "https://example.com"
	timeout = 60 * time.Second
)

//go:embed domino-lib/*js
var lib embed.FS

//go:embed domintf.js
var domIntfJs embed.FS

//go:embed regen-runtime.js
var regenRtJs embed.FS

var domIntf string
var regenRt string

func init() {
	data, err := domIntfJs.ReadFile("domintf.js")
	if err != nil {
		panic(err.Error())
	}
	domIntf = string(data)
	data, err = regenRtJs.ReadFile("regen-runtime.js")
	if err != nil {
		panic(err.Error())
	}
	regenRt = string(data)
}

type MutationType int

const (
	Value   = 1
	Attr    = 2
	RmAttr  = 3
	Rm      = 4
	Mv      = 5
	Insert  = 6
)

func (t MutationType) String() string {
	switch t {
	case Value:
		return "Value"
	case Attr:
		return "Attr"
	case RmAttr:
		return "RmAttr"
	case Rm:
		return "Rm"
	case Mv:
		return "Mv"
	case Insert:
		return "Insert"
	}
	return ""
}

type Mutation struct {
	Time time.Time
	Type MutationType
	Path string
	Tag  string
	Node map[string]string
}

type Domino struct {
	loop       *eventloop.EventLoop
	html       string
	outputHtml string
	domChange  chan Mutation
	query      func(sel, prop string) (val string, err error)
	xhrq       func(req *http.Request) (resp *http.Response, err error)
}

func NewDomino(
	html string,
	xhr func(req *http.Request) (resp *http.Response, err error),
	query func(sel, prop string) (val string, err error),
) (d *Domino) {
	d = &Domino{
		html:      html,
		xhrq:      xhr,
		domChange: make(chan Mutation, 100),
		query:     query,
	}
	return
}

func (d *Domino) Start() {
	log.Printf("Start event loop")
	d.loop = eventloop.NewEventLoop()

	d.loop.Start()
	log.Printf("event loop started")
}

func (d *Domino) Stop() {
	d.loop.Stop()
}

func IntrospectError(err error, script string) {
	prefix := "Line "
	i := strings.Index(err.Error(), prefix)
	if i > 0 {
		i += len(prefix)
		s := err.Error()[i:]
		yxStart := strings.Split(s, " ")[0]
		yx := strings.Split(yxStart, ":")
		y, _ := strconv.Atoi(yx[0])
		x, _ := strconv.Atoi(yx[1])
		lines := strings.Split(script, "\n")

		if y-1 > len(lines)-1 {
			y = len(lines)
		}

		if wholeLine := lines[y-1]; len(wholeLine) > 100 {
			from := x - 50
			to := x + 50
			if from < 0 {
				from = 0
			}
			if to >= len(wholeLine) {
				to = len(wholeLine) - 1
			}
			log.Printf("the line: %v", wholeLine[from:to])
		} else {
			if y > 0 && len(lines[y-1]) < 120 {
				log.Printf("%v: %v", y-1, lines[y-1])
			}
			if y < len(lines) {
				log.Printf("%v: %v", y, lines[y])
			}
			if y+1 < len(lines) && len(lines[y+1]) < 120 {
				log.Printf("%v: %v", y+1, lines[y+1])
			}
		}
	}
}

func printCode(code string, maxWidth int) {
	if maxWidth > len(code) {
		maxWidth = len(code)
	}
	log.Printf("js code: %v", code[:maxWidth])
}

func srcLoader(fn string) ([]byte, error) {
	path := filepath.FromSlash(fn)
	if !strings.Contains(path, "domino-lib/") || !strings.HasSuffix(path, ".js") {
		return nil, require.ModuleFileDoesNotExistError
	}
	data, err := lib.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.EISDIR) {
			err = require.ModuleFileDoesNotExistError
		} else {
			log.Printf("srcLoader: handling of require('%v') is not implemented", fn)
		}
	}
	return data, err
}

func (d *Domino) Exec(script string, initial bool) (res string, err error) {
	r := regexp.MustCompile(`^\s*<!--`)
	rr := regexp.MustCompile(`-->\s*$`)
	script = r.ReplaceAllString(script, "//")
	script = rr.ReplaceAllString(script, "//")
	SCRIPT := domIntf + regenRt + script
	if !initial {
		SCRIPT = script
	}

	ready := make(chan goja.Value)
	errCh := make(chan error)
	intCh := make(chan int)
	go func() {
		d.loop.RunOnLoop(func(vm *goja.Runtime) {
			log.Printf("RunOnLoop")

			if initial {
				vm.SetParserOptions(parser.WithDisableSourceMaps)

				// find domino-lib folder
				registry := require.NewRegistry(
					require.WithGlobalFolders("."),
					require.WithLoader(
						require.SourceLoader(srcLoader),
					),
				)

				console.Enable(vm)
				registry.Enable(vm)

				type S struct {
					Buf      string                                                                `json:"buf"`
					HTML     string                                                                `json:"html"`
					Origin   string                                                                `json:"origin"`
					Referrer func() string                                                         `json:"referrer"`
					Style    func(string, string, string, string) string                           `json:"style"`
					XHR      func(string, string, map[string]string, string, func(string, string)) `json:"xhr"`
					Mutated  func(t int, target string, tag string, node map[string]string)        `json:"mutated"`
					Btoa     func([]byte) string                                                   `json:"btoa"`
				}

				vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
				vm.Set("opossum", S{
					HTML:     d.html,
					Origin:   origin,
					Referrer: func() string { return origin },
					Style: func(sel, pseudo, prop, prop2 string) string {
						v, err := d.query(sel, prop)
						if err != nil {
							log.Printf("gojajs: domino: query %v: %v", sel, err)
							return ""
						}
						return v
					},
					XHR:     d.xhr,
					Mutated: d.mutated,
					Btoa:    Btoa,
				})
			}

			go func() {
				for _ = range intCh {
					vm.Interrupt("halt")
				}
			}()

			vv, err := vm.RunString(SCRIPT)
			if err != nil {
				IntrospectError(err, script)
				errCh <- fmt.Errorf("run program: %w", err)
			} else {
				ready <- vv
			}
		})
	}()

	for {
		select {
		case v := <-ready:
			log.Printf("ready")
			<-time.After(10 * time.Millisecond)
			if v != nil {
				res = v.String()
			}
			goto cleanup
		case er := <-errCh:
			log.Printf("err")
			<-time.After(10 * time.Millisecond)
			err = fmt.Errorf("event loop: %w", er)
			goto cleanup
		case <-time.After(timeout):
			log.Printf("Interrupt JS after %v", timeout)
			intCh <- 1
		}
	}

cleanup:
	close(ready)
	close(errCh)
	close(intCh)

	return
}

func Btoa(bs []byte) string {
	return base64.StdEncoding.EncodeToString(bs)
}

func (d *Domino) Exec56(script string, initial bool) (res string, err error) {
	if convert6to5 != "" {
		return d.Exec6(script, initial)
	} else {
		return d.Exec(script, initial)
	}
}

func (d *Domino) Exec6(script string, initial bool) (res string, err error) {
	cmd := exec.Command("6to5")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("6to5: %w", err)
	}
	return d.Exec(out.String(), initial)
}

// CloseDoc fires DOMContentLoaded to trigger $(document).ready(..)
func (d *Domino) CloseDoc() (err error) {
	_, err = d.Exec("if (this.document) document.close();", false)
	return
}

// TriggerClick, and return the result html
// ...then HTML5 parse it, diff the node tree
// (probably faster and cleaner than anything else)
func (d *Domino) TriggerClick(selector string) (newHTML string, ok bool, err error) {
	res, err := d.Exec(`
		var sel = '`+selector+`';
		var el = document.querySelector(sel);
		var fn;

		console.log('gojafs: query ' + sel);

		if (!el) {
			console.log('el is null/undefined');
			null;
		} else if (el._listeners && el._listeners.click && (fn = el.click.bind(el))) {
			fn();
			true;
		} else if (el._handlers && el._handlers.click && (fn = el.click.bind(el))) {
			fn();
			true;
		} else if (el.type === 'submit' || el.type === 'button') {
			let p;
			let submitted = false;
			for (p = el; p = p.parentElement; p != null) {
				if (p.tagName && p.tagName === 'FORM') {
					const event = new Event('submit');
					event.cancelable = true;
					if (p.onsubmit) p.onsubmit(event);
					if (!event.defaultPrevented) {
						p.submit();
					}
					submitted = true;
					break;
				}
			}
			submitted;
		} else {
			console.log('   no handler found');
			false;
		}
	`, false)

	if ok = res == "true"; ok {
		newHTML, ok, err = d.TrackChanges()
	}

	return
}

// Put change into html (e.g. from input field mutation)
func (d *Domino) PutAttr(selector, attr, val string) (ok bool, err error) {
	res, err := d.Exec(`
		var sel = '`+selector+`';
		var el = document.querySelector(sel);
		el.attr('`+attr+`', '`+val+`');
		!!el;
	`, false)

	ok = res == "true"

	return
}

func (d *Domino) TrackChanges() (html string, changed bool, err error) {
outer:
	for {
		// TODO: either add other change types like ajax begin/end or
		// just have one channel for all events worth waiting for.
		select {
		case m := <-d.domChange:
			changed = true
			if strings.ToLower(m.Tag) == "script" {
				s := ""
				src, ok := m.Node["src"]
				if ok {
					ch := make(chan string)
					log.Printf("<script> GET %v", src)
					d.xhr("GET", src, make(map[string]string), "", func(data, err string) {
						if err != "" {
							log.Printf("xhr %v: %v", src, err)
							log.Printf("data: %v", data)
						}
						ch <- data
					})
					s = <-ch
				} else if inner, ok := m.Node["innerHTML"]; ok {
					s = inner
				}
				if strings.TrimSpace(s) != "" {
					sp := s
					if len(sp) > 20 {
						sp = sp[:20] + "..."
					}
					if _, err := d.Exec56(s, false); err != nil {
						log.Printf("exec %v: %v", src, err)
					}
				}
			}
		case <-time.After(time.Second):
			break outer
		}
	}

	if changed {
		html, err = d.Exec("document.querySelector('html').innerHTML;", false)
		if err != nil {
			return
		}
	}
	d.outputHtml = html
	return
}

func (d *Domino) xhr(method, uri string, h map[string]string, data string, cb func(data string, err string)) {
	uri = strings.TrimPrefix(uri, ".")
	if !strings.HasPrefix(uri, "http") && !strings.HasPrefix(uri, "/") {
		// TODO: use instead origin/url prefix
		uri = "/"+uri
	}
	req, err := http.NewRequest(method /*u.String()*/, uri, strings.NewReader(data))
	if err != nil {
		err = fmt.Errorf("new http req: %v", err)
		cb("", err.Error())
		return
	}
	for k, v := range h {
		req.Header.Add(k, v)
	}
	go func() {
		resp, err := d.xhrq(req)
		if err != nil {
			err = fmt.Errorf("xhrq: %v", err)
			cb("", err.Error())
			return
		}
		//defer resp.Body.Close()
		bs, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			err = fmt.Errorf("read all: %v", err)
			cb("", err.Error())
			return
		}
		d.loop.RunOnLoop(func(*goja.Runtime) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("recovered in xhr: %v", r)
				}
			}()
			cb(string(bs), "")
		})
	}()
}

func (d *Domino) mutated(t int, q string, tag string, n map[string]string) {
	q = strings.Replace(q, "undefined", "", 1)
	m := Mutation{
		Time: time.Now(),
		Type: MutationType(t),
		Path: q,
		Tag: tag,
		Node: n,
	}

	select {
	case d.domChange <- m:
	default:
		log.Printf("dom changes backlog full")
	}
}

func (d *Domino) docPath(path string) (dp string, err error) {
	if !strings.HasPrefix(path, "/0") {
		return "", fmt.Errorf("malformed path %v", path)
	}
	path = strings.TrimPrefix(path, "/0")
	q := "document.getElementsByTagName('body')[0]"
	for _, el := range strings.Split(path, "/") {
		if el == "" {
			continue
		}
		i, err := strconv.Atoi(el)
		if err == nil {
			q += fmt.Sprintf(".children[%d]", i)
		} else {
			q += fmt.Sprintf(".%v", el)
		}
	}
	return q, nil
}

func (d *Domino) Retrieve(path string) string {
	tmp := strings.Split(path, "/")
	path = strings.Join(tmp[:len(tmp)-1], "/")
	k := tmp[len(tmp)-1]
	if !strings.HasPrefix(path, "/0") {
		log.Printf("malformed path %v", path)
		return ""
	}
	dp, err := d.docPath(path)
	if err != nil {
		log.Printf("doc path %v: %v", path, err)
		return ""
	}
	res, err := d.Exec(dp+"."+k, false)
	if err != nil {
		log.Printf("exec %v: %v", dp+"."+k, err)
		return ""
	}
	return res
}

func (d *Domino) Write(path, val string) (err error) {
	tmp := strings.Split(path, "/")
	path = strings.Join(tmp[:len(tmp)-1], "/")
	k := tmp[len(tmp)-1]
	if !strings.HasPrefix(path, "/0") {
		return fmt.Errorf("malformed path %v", path)
	}
	dp, err := d.docPath(path)
	if err != nil {
		return fmt.Errorf("doc path %v: %v", path, err)
	}
	_, err = d.Exec(dp+`.`+k+` = '`+val+`'`, false)
	return
}

func (d *Domino) List(path string) (l []string) {
	l = make([]string, 0, 10)
	dp, err := d.docPath(path)
	if err != nil {
		log.Printf("doc path %v: %v", path, err)
		return
	}
	q := `
	(function() {
		let items = [];
		let q = ` + dp + `;
		let ks = getProperties(q);
		let ms = getMethods(q);
		let i;
		if (q.children) {
			for (i = 0; i < q.children.length; i++) {
				items.push(i);
			}
		}
		for (i = 0; i < ks.length; i++) {
			items.push(ks[i]);
		}
		for (i = 0; i < ms.length; i++) {
			items.push(ms[i] + '()');
		}
		return items;
	})()
	`
	res, err := d.Exec(q, false)
	if err != nil {
		log.Printf("exec %v: %v", q, err)
		return
	}
	return strings.Split(res, ",")
}

// AJAX:
// https://stackoverflow.com/questions/7086858/loading-ajax-app-with-jsdom

// Babel on Goja:
// https://github.com/dop251/goja/issues/5#issuecomment-259996573

// Goja supports ES5.1 which is essentially JS assembly:
// https://github.com/dop251/goja/issues/76#issuecomment-399253779
