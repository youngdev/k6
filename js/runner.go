package js

import (
	log "github.com/Sirupsen/logrus"
	"github.com/loadimpact/speedboat/lib"
	"github.com/loadimpact/speedboat/stats"
	"github.com/robertkrimen/otto"
	"golang.org/x/net/context"
	"math"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sync"
)

type Runner struct {
	filename string
	source   string

	logger *log.Logger
}

type VU struct {
	Runner *Runner
	VM     *otto.Otto
	Script *otto.Script

	Collector *stats.Collector

	Client      http.Client
	FollowDepth int

	ID        int64
	Iteration int64
}

func New(filename, source string) *Runner {
	return &Runner{
		filename: filename,
		source:   source,
		logger: &log.Logger{
			Out:       os.Stderr,
			Formatter: &log.TextFormatter{},
			Hooks:     make(log.LevelHooks),
			Level:     log.DebugLevel,
		},
	}
}

func (r *Runner) NewVU() (lib.VU, error) {
	vuVM := otto.New()

	script, err := vuVM.Compile(r.filename, r.source)
	if err != nil {
		return nil, err
	}

	vu := VU{
		Runner: r,
		VM:     vuVM,
		Script: script,

		Collector: stats.NewCollector(),

		Client: http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: math.MaxInt32,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errInternalHandleRedirect
			},
		},
		FollowDepth: 10,
	}

	vu.VM.Set("$http", map[string]interface{}{
		"request": func(call otto.FunctionCall) otto.Value {
			method, _ := call.Argument(0).ToString()
			url, _ := call.Argument(1).ToString()

			body, isForm, err := bodyFromValue(call.Argument(2))
			if err != nil {
				panic(call.Otto.MakeTypeError("invalid body"))
			}

			params, err := paramsFromObject(call.Argument(3).Object())
			if err != nil {
				panic(err)
			}

			headers := make(map[string]string, len(params.Headers))
			for key, val := range params.Headers {
				headers[http.CanonicalHeaderKey(key)] = val
			}
			if _, ok := headers["Content-Type"]; !ok {
				if isForm {
					headers["Content-Type"] = "application/x-www-form-urlencoded"
				}
			}
			params.Headers = headers

			res, err := vu.HTTPRequest(method, url, body, params, 0)
			if err != nil {
				panic(jsCustomError(call.Otto, "HTTPError", err))
			}

			val, err := res.ToValue(call.Otto)
			if err != nil {
				panic(jsError(call.Otto, err))
			}

			return val
		},
		"batch": func(call otto.FunctionCall) otto.Value {
			obj := call.Argument(0).Object()
			if obj == nil {
				panic(call.Otto.MakeTypeError("first argument must be an object/array"))
			}

			wg := sync.WaitGroup{}
			mutex := sync.Mutex{}
			for _, key := range obj.Keys() {
				v, _ := obj.Get(key)

				var method string
				var url string
				var body string
				var params HTTPParams

				switch {
				case v.IsString():
					method = "GET"
					url = v.String()
				case v.IsObject():
					o := v.Object()

					keys := o.Keys()
					if len(keys) == 1 {
						method = "GET"
						urlV, _ := o.Get(keys[0])
						url = urlV.String()
						break
					}

					for _, key := range keys {
						switch key {
						case "0":
							v, _ := o.Get(key)
							method = v.String()
						case "1":
							v, _ := o.Get(key)
							url = v.String()
						case "2":
							v, _ := o.Get(key)
							body = v.String()
						case "3":
							v, _ := o.Get(key)
							paramsObj := v.Object()
							if paramsObj == nil {
								panic(call.Otto.MakeTypeError("params must be an object"))
							}
							params, err = paramsFromObject(paramsObj)
							if err != nil {
								panic(jsError(call.Otto, err))
							}
						}
					}
				}

				wg.Add(1)
				go func() {
					defer wg.Done()

					res, err := vu.HTTPRequest(method, url, body, params, 0)

					mutex.Lock()
					defer mutex.Unlock()

					if err != nil {
						obj.Set(key, jsError(call.Otto, err))
						return
					}

					val, err := res.ToValue(call.Otto)
					if err != nil {
						obj.Set(key, jsError(call.Otto, err))
						return
					}

					obj.Set(key, val)
				}()
			}
			wg.Wait()

			return obj.Value()
		},
		// "setMaxConnsPerHost": func(call otto.FunctionCall) otto.Value {
		// 	num, err := call.Argument(0).ToInteger()
		// 	if err != nil {
		// 		panic(call.Otto.MakeTypeError("argument must be an integer"))
		// 	}
		// 	if num <= 0 {
		// 		panic(call.Otto.MakeRangeError("argument must be >= 1"))
		// 	}
		// 	if num > math.MaxInt32 {
		// 		num = math.MaxInt32
		// 	}

		// 	vu.Client.MaxConnsPerHost = int(num)

		// 	return otto.UndefinedValue()
		// },
	})
	vu.VM.Set("$vu", map[string]interface{}{
		"sleep": func(call otto.FunctionCall) otto.Value {
			t, _ := call.Argument(0).ToFloat()
			vu.Sleep(t)
			return otto.UndefinedValue()
		},
		"id": func(call otto.FunctionCall) otto.Value {
			val, err := call.Otto.ToValue(vu.ID)
			if err != nil {
				panic(jsError(call.Otto, err))
			}
			return val
		},
		"iteration": func(call otto.FunctionCall) otto.Value {
			val, err := call.Otto.ToValue(vu.Iteration)
			if err != nil {
				panic(jsError(call.Otto, err))
			}
			return val
		},
	})
	vu.VM.Set("$test", map[string]interface{}{
		"env": func(call otto.FunctionCall) otto.Value {
			key, _ := call.Argument(0).ToString()

			value, ok := os.LookupEnv(key)
			if !ok {
				return otto.UndefinedValue()
			}

			val, err := call.Otto.ToValue(value)
			if err != nil {
				panic(jsError(call.Otto, err))
			}
			return val
		},
		"abort": func(call otto.FunctionCall) otto.Value {
			panic(lib.AbortTest)
			return otto.UndefinedValue()
		},
	})
	vu.VM.Set("$log", map[string]interface{}{
		"log": func(call otto.FunctionCall) otto.Value {
			level, _ := call.Argument(0).ToString()
			msg, _ := call.Argument(1).ToString()

			fields := make(map[string]interface{})
			fieldsObj := call.Argument(2).Object()
			if fieldsObj != nil {
				for _, key := range fieldsObj.Keys() {
					valObj, _ := fieldsObj.Get(key)
					val, err := valObj.Export()
					if err != nil {
						panic(jsError(call.Otto, err))
					}
					fields[key] = val
				}
			}

			vu.Log(level, msg, fields)

			return otto.UndefinedValue()
		},
	})

	init := `
	function HTTPResponse() {
		this.json = function() {
			return JSON.parse(this.body);
		};
	}
	
	$http.get = function(url, data, params) { return $http.request('GET', url, data, params); };
	$http.head = function(url, data, params) { return $http.request('HEAD', url, data, params); };
	$http.post = function(url, data, params) { return $http.request('POST', url, data, params); };
	$http.put = function(url, data, params) { return $http.request('PUT', url, data, params); };
	$http.patch = function(url, data, params) { return $http.request('PATCH', url, data, params); };
	$http.delete = function(url, data, params) { return $http.request('DELETE', url, data, params); };
	$http.options = function(url, data, params) { return $http.request('OPTIONS', url, data, params); };
	
	$log.debug = function(msg, fields) { $log.log('debug', msg, fields); };
	$log.info = function(msg, fields) { $log.log('info', msg, fields); };
	$log.warn = function(msg, fields) { $log.log('warn', msg, fields); };
	$log.error = function(msg, fields) { $log.log('error', msg, fields); };
	`
	if _, err := vu.VM.Eval(init); err != nil {
		return nil, err
	}

	return &vu, nil
}

func (u *VU) Reconfigure(id int64) error {
	u.ID = id
	u.Iteration = 0

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	u.Client.Jar = jar

	return nil
}

func (u *VU) RunOnce(ctx context.Context) error {
	u.Iteration++
	if _, err := u.VM.Run(u.Script); err != nil {
		return err
	}
	return nil
}
