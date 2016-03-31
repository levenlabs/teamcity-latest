package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/levenlabs/go-llog"
	"github.com/levenlabs/go-srvclient"
	"github.com/mediocregopher/lever"
	"github.com/mediocregopher/skyapi/client"
)

var (
	restUser, restPass, restAddr string
	listenAddr                   string
	skyapiAddr                   string
)

type requestParams struct {
	ip            string
	buildTypeID   string
	tag           string
	artifactName  string
	latestBuildID string
}

func (r requestParams) log(f llog.LogFunc, msg string, kvs ...llog.KV) {
	var kv llog.KV
	if len(kvs) > 0 {
		kv = kvs[0]
	} else {
		kv = llog.KV{}
	}
	kv["ip"] = r.ip
	kv["buildTypeID"] = r.buildTypeID
	if r.tag != "" {
		kv["tag"] = r.tag
	}
	kv["artifactName"] = r.artifactName
	if r.latestBuildID != "" {
		kv["latestBuildID"] = r.latestBuildID
	}
	f(msg, kv)
}

func main() {
	l := lever.New("teamcity-latest", nil)
	l.Add(lever.Param{
		Name:        "--rest-user",
		Description: "Username to authenticate to the rest api as",
	})
	l.Add(lever.Param{
		Name:        "--rest-pass",
		Description: "Password to authenticate to the rest api with",
	})
	l.Add(lever.Param{
		Name:        "--rest-addr",
		Description: "Address the rest api is listening on",
		Default:     "http://localhost:8111",
	})
	l.Add(lever.Param{
		Name:        "--listen-addr",
		Description: "Address to listen for requests on",
		Default:     ":8112",
	})
	l.Add(lever.Param{
		Name:        "--skyapi-addr",
		Description: "Hostname of skyapi, to be looked up via a SRV request. Unset means don't register with skyapi",
	})
	l.Add(lever.Param{
		Name:        "--log-level",
		Description: "Minimum log level to show, either debug, info, warn, error, or fatal",
		Default:     "info",
	})
	l.Parse()

	restUser, _ = l.ParamStr("--rest-user")
	restPass, _ = l.ParamStr("--rest-pass")
	restAddr, _ = l.ParamStr("--rest-addr")
	listenAddr, _ = l.ParamStr("--listen-addr")
	skyapiAddr, _ = l.ParamStr("--skyapi-addr")

	logLevel, _ := l.ParamStr("--log-level")
	llog.SetLevelFromString(logLevel)

	if skyapiAddr != "" {
		actualSkyapiAddr, err := srvclient.SRV(skyapiAddr)
		if err != nil {
			llog.Fatal("couldn't look up skyapi address", llog.KV{"skyapiAddr": skyapiAddr, "err": err})
		}

		go func() {
			err := client.Provide(
				actualSkyapiAddr, "teamcity-latest", listenAddr, 1, 100,
				3, 15*time.Second,
			)
			llog.Fatal("skapi client failed", llog.KV{"skyapiAddr": actualSkyapiAddr, "err": err})
		}()
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req requestParams
		req.ip = r.RemoteAddr

		parts := strings.Split(r.URL.Path[1:], "/")
		if len(parts) < 2 {
			req.log(llog.Warn, "invalid url, not enough parts", llog.KV{"url": r.URL.Path})
			http.Error(w, "invalid url, must be /buildTypeID/[tag]/artifactName", 400)
			return
		}
		req.buildTypeID = parts[0]
		if len(parts) == 3 {
			req.tag = parts[1]
			req.artifactName = parts[2]
		} else {
			req.artifactName = parts[1]
		}

		if req.buildTypeID == "" || req.artifactName == "" {
			req.log(llog.Warn, "invalid url, empty parts", llog.KV{"url": r.URL.Path})
			http.Error(w, "invalid url, must be /buildTypeID/[tag]/artifactName", 400)
			return
		}

		// if they sent a query include that as well
		if r.URL.RawQuery != "" {
			req.artifactName += "?" + r.URL.RawQuery
		}

		req.log(llog.Info, "request")

		var err error
		req.latestBuildID, err = latestBuildID(req.buildTypeID, req.tag)
		if err != nil {
			req.log(llog.Error, "couldn't get last build id", llog.KV{"err": err})
			http.Error(w, err.Error(), 500)
			return
		}

		if remoteHash := r.Header.Get("If-None-Match"); remoteHash != "" {
			tcHash, err := artifactHash(req.latestBuildID, req.artifactName)
			if err != nil {
				req.log(llog.Error, "couldn't check hash", llog.KV{"err": err})
				http.Error(w, fmt.Sprintf("Could not check hash: %s", err), 500)
				return
			}
			if tcHash == remoteHash {
				req.log(llog.Info, "hashes match, not retrieving")
				w.WriteHeader(304)
				return
			}
		}

		rc, contentLen, err := buildDownload(req.latestBuildID, req.artifactName)
		if err != nil {
			req.log(llog.Info, "couldn't get build download", llog.KV{"err": err})
			http.Error(w, err.Error(), 500)
			return
		}
		defer rc.Close()

		w.Header().Set("Content-Length", strconv.FormatInt(contentLen, 10))
		io.Copy(w, rc)
	})

	llog.Info("listening", llog.KV{"addr": listenAddr})
	err := http.ListenAndServe(listenAddr, nil)
	llog.Fatal("error listening", llog.KV{"err": err})
}

func latestBuildID(buildTypeID, tag string) (string, error) {
	//status:SUCCESS means it succeeded
	//branch:default:any means it can come from any branch
	//count:1 means return the latest match only
	l := []string{"status:SUCCESS", "branch:default:any", "count:1"}
	//buildType:id:{id} will only return builds for the buildTypeID
	l = append(l, fmt.Sprintf("buildType:id:%s", buildTypeID))
	//if a tag was sent then filter to builds including this tag(s)
	if tag != "" {
		l = append(l, fmt.Sprintf("tag:%s", tag))
	}
	u := fmt.Sprintf(
		"%s/httpAuth/app/rest/builds/?locator=%s",
		restAddr,
		strings.Join(l, ","),
	)

	r, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	r.SetBasicAuth(restUser, restPass)
	r.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	out := struct {
		Builds []struct {
			ID int `json:"id"`
		} `json:"build"`
	}{}

	if err := json.Unmarshal(body, &out); err != nil {
		return "", errors.New(string(body))
	}

	if len(out.Builds) < 1 {
		return "", fmt.Errorf("no builds with tag '%s' found", tag)
	}

	return strconv.Itoa(out.Builds[0].ID), nil
}

func artifactHash(id, artifactName string) (string, error) {
	u := fmt.Sprintf(
		"%s/httpAuth/app/rest/builds/id:%s/artifacts/content/%s.md5",
		restAddr,
		id,
		artifactName,
	)

	r, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	r.SetBasicAuth(restUser, restPass)

	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	berr, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(berr)), nil
}

// the ReadCloser *must* be closed when done
func buildDownload(id, artifactName string) (io.ReadCloser, int64, error) {
	u := fmt.Sprintf(
		"%s/httpAuth/app/rest/builds/id:%s/artifacts/content/%s",
		restAddr,
		id,
		artifactName,
	)

	r, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	r.SetBasicAuth(restUser, restPass)

	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return nil, 0, err
	}

	if resp.ContentLength < 0 {
		berr, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, 0, err
		}
		resp.Body.Close()
		return nil, 0, errors.New(string(berr))
	}

	return resp.Body, resp.ContentLength, nil
}
