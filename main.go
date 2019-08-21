// Copyright (c) 2019 Sorint.lab S.p.A.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"crypto/tls"
	b64 "encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/ercole-io/ercole-agent-rhel5/config"
	"github.com/ercole-io/ercole-agent-rhel5/marshal"
	"github.com/ercole-io/ercole-agent-rhel5/model"
	"github.com/ercole-io/ercole-agent-rhel5/scheduler"
	"github.com/ercole-io/ercole-agent-rhel5/scheduler/storage"
)

func main() {
	configuration := config.ReadConfig()

	buildData(configuration) // first run

	memStorage := storage.NewMemoryStorage()
	scheduler := scheduler.New(memStorage)
	_, err := scheduler.RunEvery(time.Duration(configuration.Frequency)*time.Hour, buildData, configuration)

	if err != nil {
		log.Fatal("Error sending data", err)
	}

	scheduler.Start()
	scheduler.Wait()

}

func buildData(configuration config.Configuration) {

	out := fetcher(configuration, "host")
	host := marshal.Host(out)

	host.Environment = configuration.Envtype
	host.Location = configuration.Location

	out = fetcher(configuration, "filesystem")
	filesystems := marshal.Filesystems(out)

	out = fetcher(configuration, "oratab")
	dbs := marshal.Oratab(out)

	var databases []model.Database

	for _, db := range dbs {

		out = fetcher(configuration, "dbstatus", db)
		dbStatus := strings.TrimSpace(string(out))

		if dbStatus == "OPEN" {
			out = fetcher(configuration, "dbversion", db)
			outVersion := string(out)

			dbVersion := "12"
			if strings.HasPrefix(outVersion, "11") {
				dbVersion = "11"
			} else if strings.HasPrefix(outVersion, "10") {
				dbVersion = "10"
			} else if strings.HasPrefix(outVersion, "9") {
				dbVersion = "9"
			}

			if configuration.Forcestats {
				fetcher(configuration, "stats", db, dbVersion)
			}

			out = fetcher(configuration, "db", db, dbVersion)
			database := marshal.Database(out)

			out = fetcher(configuration, "tablespace", db, dbVersion)
			database.Tablespaces = marshal.Tablespaces(out)

			out = fetcher(configuration, "schema", db, dbVersion)
			database.Schemas = marshal.Schemas(out)

			out = fetcher(configuration, "patch", db, dbVersion)
			database.Patches = marshal.Patches(out)

			out = fetcher(configuration, "feature", db, dbVersion)
			database.Features = marshal.Features(out)

			out = fetcher(configuration, "license", db, dbVersion, host.Type)
			database.Licenses = marshal.Licenses(out)

			out = fetcher(configuration, "addm", db, dbVersion)
			database.ADDMs = marshal.Addms(out)

			out = fetcher(configuration, "segmentadvisor", db, dbVersion)
			database.SegmentAdvisors = marshal.SegmentAdvisor(out)

			out = fetcher(configuration, "psu", db, dbVersion)
			database.LastPSUs = marshal.PSU(out)

			out = fetcher(configuration, "backup", db, dbVersion)
			database.Backups = marshal.Backups(out)

			databases = append(databases, database)
		} else if dbStatus == "MOUNTED" {
			out = fetcher(configuration, "dbmounted", db)
			database := marshal.Database(out)
			database.Tablespaces = []model.Tablespace{}
			database.Schemas = []model.Schema{}
			database.Patches = []model.Patch{}
			database.Features = []model.Feature{}
			database.Licenses = []model.License{}
			database.ADDMs = []model.Addm{}
			database.SegmentAdvisors = []model.SegmentAdvisor{}
			database.LastPSUs = []model.PSU{}
			database.Backups = []model.Backup{}

			databases = append(databases, database)
		}
	}

	hostData := new(model.HostData)

	extraInfo := new(model.ExtraInfo)
	extraInfo.Filesystems = filesystems

	extraInfo.Databases = databases

	hostData.Extra = *extraInfo

	hostData.Info = host
	hostData.Hostname = host.Hostname
	// override host name with the one in config if != default
	if configuration.Hostname != "default" {
		hostData.Hostname = configuration.Hostname
	}
	hostData.Environment = configuration.Envtype
	hostData.Location = configuration.Location

	// Fill index fields
	hdDatabases := ""
	hdSchemas := ""
	for _, db := range databases {
		hdDatabases += db.Name + " "
		for _, sc := range db.Schemas {
			hdSchemas += sc.User + " "
		}
	}
	hdDatabases = strings.TrimSpace(hdDatabases)
	hostData.Databases = hdDatabases

	hdSchemas = strings.TrimSpace(hdSchemas)
	hostData.Schemas = hdSchemas

	sendData(hostData, configuration)

}

func sendData(data *model.HostData, configuration config.Configuration) {

	log.Println("Sending data...")

	b, _ := json.Marshal(data)
	s := string(b)

	log.Println("Data:", s)

	client := &http.Client{}

	//Disable certificate validation if enableServerValidation is false
	if configuration.EnableServerValidation == false {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	req, err := http.NewRequest("POST", configuration.Serverurl, bytes.NewReader(b))
	req.Header.Add("Content-Type", "application/json")
	auth := configuration.Serverusr + ":" + configuration.Serverpsw
	authEnc := b64.StdEncoding.EncodeToString([]byte(auth))
	req.Header.Add("Authorization", "Basic "+authEnc)
	resp, err := client.Do(req)

	sendResult := "FAILED"

	if err != nil {
		log.Println("Error sending data", err)
	} else {
		log.Println("Response status:", resp.Status)
		if resp.StatusCode == 200 {
			sendResult = "SUCCESS"
		}
		defer resp.Body.Close()
	}

	log.Println("Sending result:", sendResult)

}

func fetcher(configuration config.Configuration, fetcherName string, params ...string) []byte {
	var (
		cmd    *exec.Cmd
		err    error
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	log.Println("Fetching", fetcherName)

	baseDir := config.GetBaseDir()
	cmd = exec.Command(baseDir+"/fetch/"+fetcherName, params...)

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if len(stderr.Bytes()) > 0 {
		log.Print(string(stderr.Bytes()))
	}

	if err != nil {
		if fetcherName != "dbstatus" {
			log.Fatal(err)
		} else {
			return []byte("UNREACHABLE") // fallback
		}
	}

	return stdout.Bytes()
}
