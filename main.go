package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/levenlabs/go-srvclient"
	"github.com/mediocregopher/lever"
	"github.com/mediocregopher/skyapi/client"
)

var (
	restUser, restPass, restAddr string
	listenAddr                   string
	skyapiAddr                   string
)

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
	l.Parse()

	restUser, _ = l.ParamStr("--rest-user")
	restPass, _ = l.ParamStr("--rest-pass")
	restAddr, _ = l.ParamStr("--rest-addr")
	listenAddr, _ = l.ParamStr("--listen-addr")
	skyapiAddr, _ = l.ParamStr("--skyapi-addr")

	if skyapiAddr != "" {
		skyapiAddr, err := srvclient.SRV(skyapiAddr)
		if err != nil {
			log.Fatal(err)
		}

		go func() {
			log.Fatal(client.Provide(
				skyapiAddr, "teamcity-latest", listenAddr, 1, 100,
				3, 15*time.Second,
			))
		}()
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path[1:], "/")
		if len(parts) < 2 {
			http.Error(w, "invalid url, must be /buildTypeID/[tag]/artifactName", 400)
			return
		}
		buildTypeID := parts[0]
		var tag string
		var artifactName string
		if len(parts) == 3 {
			tag = parts[1]
			artifactName = parts[2]
		} else {
			artifactName = parts[1]
		}

		if buildTypeID == "" || tag == "" || artifactName == "" {
			http.Error(w, "invalid url, must be /buildTypeID/[tag]/artifactName", 400)
			return
		}

		log.Printf("request for buildTypeID:%s tag:%s artifactName:%s", buildTypeID, tag, artifactName)

		id, err := latestBuildID(buildTypeID, tag)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		if remoteHash := r.Header.Get("If-None-Match"); remoteHash != "" {
			tcHash, err := artifactHash(id, artifactName)
			if err != nil {
				http.Error(w, fmt.Sprintf("Could not check hash: %s", err), 500)
				return
			}
			if tcHash == remoteHash {
				w.WriteHeader(304)
				return
			}
		}

		rc, contentLen, err := buildDownload(id, artifactName)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rc.Close()

		w.Header().Set("Content-Length", strconv.FormatInt(contentLen, 10))
		io.Copy(w, rc)
	})

	log.Printf("listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
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
