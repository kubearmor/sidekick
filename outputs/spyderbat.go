package outputs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/google/uuid"
	"github.com/kubearmor/sidekick/types"
)

func isSourcePresent(config *types.Configuration) (bool, error) {

	client := &http.Client{}

	source_url, err := url.JoinPath(config.Spyderbat.APIUrl, "api/v1/org/"+config.Spyderbat.OrgUID+"/source/")
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest("GET", source_url, new(bytes.Buffer))
	if err != nil {
		return false, err
	}
	req.Header.Add("Authorization", "Bearer "+config.Spyderbat.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, errors.New("HTTP error: " + resp.Status)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	var sources []map[string]interface{}
	if err := json.Unmarshal(body, &sources); err != nil {
		return false, err
	}
	uid := "kubearmor_" + config.Spyderbat.OrgUID
	for _, source := range sources {
		if id, ok := source["uid"]; ok && id.(string) == uid {
			return true, nil
		}
	}
	return false, nil
}

type SourceBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	UID         string `json:"uid"`
}

func makeSource(config *types.Configuration) error {

	data := SourceBody{
		Name:        config.Spyderbat.Source,
		Description: config.Spyderbat.SourceDescription,
		UID:         "kubearmor_" + config.Spyderbat.OrgUID,
	}
	body := new(bytes.Buffer)
	if err := json.NewEncoder(body).Encode(data); err != nil {
		return err
	}

	client := &http.Client{}

	source_url, err := url.JoinPath(config.Spyderbat.APIUrl, "api/v1/org/"+config.Spyderbat.OrgUID+"/source/")
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", source_url, body)
	if err != nil {
		return err
	}
	req.Header.Add("Authorization", "Bearer "+config.Spyderbat.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			if b, err := ioutil.ReadAll(resp.Body); err == nil {
				return errors.New("Bad request: " + string(b))
			}
		}
		return errors.New("HTTP error: " + resp.Status)
	}
	defer resp.Body.Close()

	return nil
}

const Schema = "kubearmor_alert::1.0.0"

var PriorityMap = map[string]string{
	"types.Emergency": "critical",
	"Alert":           "high",
	"types.Critical":  "critical",
	"types.Error":     "high",
	"types.Warning":   "medium",
	"types.Notice":    "low",
	"Log":             "info",
	"types.Debug":     "info",
}

type spyderbatPayload struct {
	Schema        string   `json:"schema"`
	ID            string   `json:"id"`
	MonotonicTime int      `json:"monotonic_time"`
	OrcTime       float64  `json:"orc_time"`
	Time          float64  `json:"time"`
	PID           int32    `json:"pid"`
	Level         string   `json:"level"`
	Message       []string `json:"msg"`
	Arguments     string   `json:"args"`
	Container     string   `json:"container"`
}

func newSpyderbatPayload(kubearmorpayload types.KubearmorPayload) (spyderbatPayload, error) {
	nowTime := float64(time.Now().UnixNano()) / 1000000000

	eventTime := float64(kubearmorpayload.Timestamp / 1000000000.0)

	level := PriorityMap[kubearmorpayload.EventType]
	arguments := kubearmorpayload.OutputFields["proc.cmdline"].(string)
	container := kubearmorpayload.OutputFields["container.id"].(string)

	return spyderbatPayload{
		Schema:        Schema,
		ID:            uuid.NewString(),
		MonotonicTime: time.Now().Nanosecond(),
		OrcTime:       nowTime,
		Time:          eventTime,
		PID:           int32(kubearmorpayload.OutputFields["PID"].(int32)),
		Level:         level,
		Arguments:     arguments,
		Container:     container,
	}, nil
}

func NewSpyderbatClient(config *types.Configuration, stats *types.Statistics, promStats *types.PromStatistics,
	statsdClient, dogstatsdClient *statsd.Client) (*Client, error) {

	hasSource, err := isSourcePresent(config)
	if err != nil {
		log.Printf("[ERROR] : Spyderbat - %v\n", err.Error())
		return nil, ErrClientCreation
	}
	if !hasSource {
		if err := makeSource(config); err != nil {
			if hasSource, err2 := isSourcePresent(config); err2 != nil || !hasSource {
				log.Printf("[ERROR] : Spyderbat - %v\n", err.Error())
				return nil, ErrClientCreation
			}
		}
	}

	source := "kubearmor_" + config.Spyderbat.OrgUID
	data_url, err := url.JoinPath(config.Spyderbat.APIUrl, "api/v1/org/"+config.Spyderbat.OrgUID+"/source/"+source+"/data/sb-agent")
	if err != nil {
		log.Printf("[ERROR] : Spyderbat - %v\n", err.Error())
		return nil, ErrClientCreation
	}
	endpointURL, err := url.Parse(data_url)
	if err != nil {
		log.Printf("[ERROR] : Spyderbat - %v\n", err.Error())
		return nil, ErrClientCreation
	}
	return &Client{
		OutputType:       "Spyderbat",
		EndpointURL:      endpointURL,
		MutualTLSEnabled: false,
		CheckCert:        true,
		ContentType:      "application/ndjson",
		Config:           config,
		Stats:            stats,
		PromStats:        promStats,
		StatsdClient:     statsdClient,
		DogstatsdClient:  dogstatsdClient,
	}, nil
}

func (c *Client) SpyderbatPost(kubearmorpayload types.KubearmorPayload) {
	c.Stats.Spyderbat.Add(Total, 1)

	c.httpClientLock.Lock()
	defer c.httpClientLock.Unlock()
	c.AddHeader("Authorization", "Bearer "+c.Config.Spyderbat.APIKey)
	c.AddHeader("Content-Encoding", "gzip")

	payload, err := newSpyderbatPayload(kubearmorpayload)
	if err == nil {
		err = c.Post(payload)
	}
	if err != nil {
		go c.CountMetric(Outputs, 1, []string{"output:spyderbat", "status:error"})
		c.Stats.Spyderbat.Add(Error, 1)
		c.PromStats.Outputs.With(map[string]string{"destination": "spyderbat", "status": Error}).Inc()
		log.Printf("[ERROR] : Spyderbat - %v\n", err.Error())
		return
	}

	go c.CountMetric(Outputs, 1, []string{"output:spyderbat", "status:ok"})
	c.Stats.Spyderbat.Add(OK, 1)
	c.PromStats.Outputs.With(map[string]string{"destination": "spyderbat", "status": OK}).Inc()
}
