package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type container struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
}

type inspect struct {
	Config struct {
		Env []string `json:"Env"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIP, HostPort string } `json:"Ports"`
		Networks map[string]struct{ IPAddress string }          `json:"Networks"`
	} `json:"NetworkSettings"`
}

// docker asks the Docker Engine for running Postgres, MySQL and MariaDB
// containers, and reads their credentials from their environment.
func docker(ctx context.Context, env Env, prefix string) []Candidate {
	if env.DockerSocket == "" || env.GOOS == "windows" {
		return nil
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return env.Dial(ctx, "unix", env.DockerSocket)
		},
	}}
	get := func(path string, v any) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("ERR900: docker answered %s", resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}

	var list []container
	if err := get("/containers/json", &list); err != nil {
		return nil
	}
	var out []Candidate
	for _, c := range list {
		img := strings.ToLower(c.Image)
		typ, port := "", 0
		switch {
		case strings.Contains(img, "postgres") || strings.Contains(img, "postgis"):
			typ, port = "postgres", 5432
		case strings.Contains(img, "mysql") || strings.Contains(img, "mariadb"):
			typ, port = "mysql", 3306
		default:
			continue
		}
		var in inspect
		if err := get("/containers/"+c.ID+"/json", &in); err != nil {
			continue
		}
		vars := map[string]string{}
		for _, e := range in.Config.Env {
			k, v, _ := strings.Cut(e, "=")
			vars[k] = v
		}
		cname := strings.TrimPrefix(firstOr(c.Names, c.ID[:12]), "/")
		f := dbFound{typ: typ, origin: "the environment of the Docker container " + cname}
		if b := in.NetworkSettings.Ports[fmt.Sprintf("%d/tcp", port)]; len(b) > 0 {
			f.host, f.port = "127.0.0.1", port
			fmt.Sscan(b[0].HostPort, &f.port)
		} else {
			for _, n := range in.NetworkSettings.Networks {
				if n.IPAddress != "" {
					f.host, f.port = n.IPAddress, port
				}
			}
			f.notes = append(f.notes, "the port is not published: reachable only from this host, at the container address")
		}
		if typ == "postgres" {
			f.user, f.db, f.password = or(vars["POSTGRES_USER"], "postgres"), or(vars["POSTGRES_DB"], vars["POSTGRES_USER"]), vars["POSTGRES_PASSWORD"]
		} else {
			f.db = or(vars["MYSQL_DATABASE"], vars["MARIADB_DATABASE"])
			f.user, f.password = or(vars["MYSQL_USER"], vars["MARIADB_USER"]), or(vars["MYSQL_PASSWORD"], vars["MARIADB_PASSWORD"])
			if f.user == "" {
				f.user, f.password = "root", or(vars["MYSQL_ROOT_PASSWORD"], vars["MARIADB_ROOT_PASSWORD"])
				f.notes = append(f.notes, "only the root password is known: better create a read-only user for laker")
			}
		}
		if f.host == "" {
			continue
		}
		out = append(out, dbCandidate(prefix, f))
	}
	return out
}

func firstOr(s []string, d string) string {
	if len(s) > 0 {
		return s[0]
	}
	return d
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
