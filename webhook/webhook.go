package webhook

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	zabbix "github.com/blacked/go-zabbix"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

var log = logrus.WithField("context", "webhook")

type WebHook struct {
	channel chan *Alert
	config  WebHookConfig
}

type WebHookConfig struct {
	Port                 int    `yaml:"port"`
	QueueCapacity        int    `yaml:"queueCapacity"`
	ZabbixServerHost     string `yaml:"zabbixServerHost"`
	ZabbixServerPort     int    `yaml:"zabbixServerPort"`
	ZabbixHostDefault    string `yaml:"zabbixHostDefault"`
	ZabbixHostAnnotation string `yaml:"zabbixHostAnnotation"`
	ZabbixKeyPrefix      string `yaml:"zabbixKeyPrefix"`
	zabbixSubpath        string `yaml:"zabbixSubpath"`
	OcpPortalAddress     string `yaml:"ocpPortalAddress"`
	EmailFrom            string `yaml:"emailFrom"`
	EmailTo              string `yaml:"emailTo"`
	AppId                string `yaml:"appId"`
	AppKey               string `yaml:"appKey"`
}

type HookRequest struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt,omitempty"`
	EndsAt       string            `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

func New(cfg *WebHookConfig) *WebHook {

	return &WebHook{
		channel: make(chan *Alert, cfg.QueueCapacity),
		config:  *cfg,
	}
}

func ConfigFromFile(filename string) (cfg *WebHookConfig, err error) {
	log.Infof("Loading configuration at '%s'", filename)
	configFile, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("can't open the config file: %s", err)
	}

	// Default values
	config := WebHookConfig{
		Port:                 8080,
		QueueCapacity:        500,
		ZabbixServerHost:     "127.0.0.1",
		ZabbixServerPort:     10051,
		ZabbixHostAnnotation: "zabbix_host",
		ZabbixKeyPrefix:      "prometheus",
		ZabbixHostDefault:    "",
		zabbixSubpath:        "/smtp",
		OcpPortalAddress:     "",
		EmailFrom:            "",
		EmailTo:              "",
		AppId:                "",
		AppKey:               "",
	}

	err = yaml.Unmarshal(configFile, &config)
	if err != nil {
		return nil, fmt.Errorf("can't read the config file: %s", err)
	}

	log.Info("Configuration loaded")
	return &config, nil
}

func (hook *WebHook) Start() error {

	// Launch the process thread
	go hook.processAlerts()

	// Launch the listening thread
	log.Println("Initializing HTTP server")
	http.HandleFunc("/alerts", hook.alertsHandler)
	http.HandleFunc("/health", hook.healthHandler)
	err := http.ListenAndServe(":"+strconv.Itoa(hook.config.Port), nil)
	if err != nil {
		return fmt.Errorf("can't start the listening thread: %s", err)
	}

	log.Info("Exiting")
	close(hook.channel)

	return nil
}

func (hook *WebHook) alertsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		hook.postHandler(w, r)
	default:
		http.Error(w, "unsupported HTTP method", 400)
	}
}

func (hook *WebHook) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	w.Write([]byte("ok"))
}

func (hook *WebHook) postHandler(w http.ResponseWriter, r *http.Request) {

	//dec := json.NewDecoder(r.Body)
	//defer r.Body.Close()

	//var m HookRequest
	//if err := dec.Decode(&m); err != nil {
	//	log.Errorf("error decoding message: %v", err)
	//	http.Error(w, "request body is not valid json", 400)
	//	return
	//}

	var m = HookRequest{}
	body, err := ioutil.ReadAll(r.Body)

	//workaround to remove quotation marks in generatorURL
	var re = regexp.MustCompile(`generatorURL":"(.+?)tab=1`)
	data := re.ReplaceAllString(string(body), `generatorURL":"`)
	log.Info("hook request: %s:", data)

	dataByte := []byte(data)
	if err == nil && dataByte != nil {
		err = json.Unmarshal(dataByte, &m)
	}

	log.Debug("http request: %v", m)
	for index := range m.Alerts {
		hook.channel <- &m.Alerts[index]
	}

}

func (hook *WebHook) processAlerts() {

	log.Info("Alerts queue started")

	// While there are alerts in the queue, batch them and send them over to Zabbix
	var metrics []*zabbix.AlertMetric
	var mailbody zabbix.MailBody
	var mailmessage zabbix.MailMessage
	var mailmsgtype zabbix.MailMessageType
	verifycode := hook.config.AppId + "_" + hook.config.AppKey + "_"
	for {
		select {
		case a := <-hook.channel:
			if a == nil {
				log.Info("Queue Closed")
				return
			}

			log.Info("incoming alert: %v", a)

			var host = ""
			if a.Labels["cluster"] == hook.config.ZabbixHostAnnotation {
				host = hook.config.ZabbixHostAnnotation
			} else {
				host = hook.config.ZabbixHostDefault
			}

			deviceIp := hook.config.OcpPortalAddress
			// Send alerts only if a host annotation is present or configuration for default host is not empty
			if host != "" {
				var alertLevel = ""
				var subject = ""
				var alertStartTime = ""
				var id = ""
				alertname := fmt.Sprintf("%s.%s", hook.config.ZabbixKeyPrefix, strings.ToLower(a.Labels["alertname"]))
				alertStatus := "0"
				if a.Status == "firing" {
					alertStatus = "1"
				}

				//Collect alert information for description and message in Annotation
				if _, ok := a.Annotations["description"]; ok {
					subject += fmt.Sprintf("%s: %s", alertname, strings.ToLower(a.Annotations["description"]))
				} else if _, ok := a.Annotations["message"]; ok {
					subject += fmt.Sprintf("%s: %s", alertname, strings.ToLower(a.Annotations["message"]))
				}

				//Process alert start time
				t, err := time.Parse(time.RFC3339, a.StartsAt)

				if err == nil {
					alertStartTime = fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d",
						t.Year(), t.Month(), t.Day(),
						t.Hour(), t.Minute(), t.Second())
					//Use alert start time as alert id
					id = alertStartTime
				}
				cluster := fmt.Sprintf("%s", strings.ToLower(a.Labels["cluster"]))
				severity := fmt.Sprintf("%s", strings.ToLower(a.Labels["severity"]))
				if cluster == "" {
					cluster = hook.config.ZabbixHostAnnotation
				}

				// classify alert level
				if severity == "critical" {
					alertLevel = "5"
				} else if severity == "warning" {
					alertLevel = "3"
				} else {
					alertLevel = "2"
				}

				log.Infof("alertMetrics: {\"data\": [{ALERTLEVEL: '%s', ALERT_START_TIME: '%s', ALERT_STATUS: '%s',"+
					"CUR_MONI_VALUE: '%s', DEVICE_IP: '%s', ID: '%s', MONI_OBJECT: '%s', SUBJECT: '%s'}]}",
					alertLevel, alertStartTime, alertStatus, alertname, deviceIp, id, cluster, subject)

				//base64 encoding for subject and alertname
				//metrics = append(metrics, zabbix.NewAlertMetric(alertLevel, alertStartTime, deviceIp, cluster,
				//	base64.StdEncoding.EncodeToString([]byte(subject)),
				//	id, alertStatus, base64.StdEncoding.EncodeToString([]byte(alertname))))

				//create mailbody and mailmessage
				mailbody = zabbix.MailBody{1,
					"" +
						"Subject：" + subject + "\n" +
						"Level：" + severity + "\n" +
						"ClusterName：" + cluster + "\n" +
						"Status：" + a.Status + "\n" +
						"Time：" + alertStartTime + "\n"}

				mailto := strings.Split(hook.config.EmailTo, ",")
				mailmessage = zabbix.MailMessage{
					hook.config.EmailFrom,
					mailto,
					[]string{},
					[]string{},
					subject,
					mailbody,
					[]string{},
				}

				mailmsgtype = zabbix.MailMessageType{mailmessage}
				//insert alertmetrics into slice
				metrics = append(metrics, zabbix.NewAlertMetric(alertStartTime, 1, 1, mailmsgtype))
				log.Infof("metrics: %v", metrics)
			}
		default:
			if len(metrics) != 0 {
				hook.alertMetricsSend(metrics, hook.config.zabbixSubpath, verifycode)
				metrics = metrics[:0]
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}
}

func (hook *WebHook) zabbixSend(metrics []*zabbix.Metric) {
	// Create instance of Packet class
	packet := zabbix.NewPacket(metrics)

	// Send packet to zabbix
	log.Infof("sending to zabbix '%s:%d'", hook.config.ZabbixServerHost, hook.config.ZabbixServerPort)
	z := zabbix.NewSender(hook.config.ZabbixServerHost, hook.config.ZabbixServerPort)
	_, err := z.Send(packet)
	if err != nil {
		log.Error(err)
	} else {
		log.Info("successfully sent metrics")
	}

}

func (hook *WebHook) zabbixAlertSend(metrics []*zabbix.AlertMetric, subpath string) {
	// Create instance of Packet class
	packet := zabbix.NewAlertPacket(metrics)

	// Send packet to zabbix
	log.Infof("sending to zabbix '%s:%d'", hook.config.ZabbixServerHost, hook.config.ZabbixServerPort)
	z := zabbix.NewSender(hook.config.ZabbixServerHost, hook.config.ZabbixServerPort)
	_, err := z.AlertSend(packet, subpath)
	if err != nil {
		log.Error(err)
	} else {
		log.Info("successfully sent alert")
	}

}

func (hook *WebHook) alertMetricsSend(metrics []*zabbix.AlertMetric, subpath string, verifycode string) {
	for _, metric := range metrics {
		// Send metric to remote server
		log.Infof("sending to remote '%s:%d'", hook.config.ZabbixServerHost, hook.config.ZabbixServerPort)
		z := zabbix.NewSender(hook.config.ZabbixServerHost, hook.config.ZabbixServerPort)
		_, err := z.AlertMetricSend(metric, subpath, verifycode)
		if err != nil {
			log.Error(err)
		} else {
			log.Info("successfully sent alert")
		}
	}
}
