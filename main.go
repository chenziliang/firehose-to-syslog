package main

import (
	"fmt"
	"log"
	"os"

	"github.com/cloudfoundry-community/firehose-to-syslog/caching"
	"github.com/cloudfoundry-community/firehose-to-syslog/eventRouting"
	"github.com/cloudfoundry-community/firehose-to-syslog/firehoseclient"
	"github.com/cloudfoundry-community/firehose-to-syslog/logging"
	"github.com/cloudfoundry-community/firehose-to-syslog/uaatokenrefresher"
	"github.com/cloudfoundry-community/go-cfclient"
	"github.com/pkg/profile"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	debug              = kingpin.Flag("debug", "Enable debug mode. This disables forwarding to syslog").Default("false").Envar("DEBUG").Bool()
	apiEndpoint        = kingpin.Flag("api-endpoint", "Api endpoint address. For bosh-lite installation of CF: https://api.10.244.0.34.xip.io").Envar("API_ENDPOINT").Required().String()
	dopplerEndpoint    = kingpin.Flag("doppler-endpoint", "Overwrite default doppler endpoint return by /v2/info").Envar("DOPPLER_ENDPOINT").String()
	syslogServer       = kingpin.Flag("syslog-server", "Syslog server.").Envar("SYSLOG_ENDPOINT").String()
	syslogProtocol     = kingpin.Flag("syslog-protocol", "Syslog protocol (tcp/udp/tcp+tls).").Default("tcp").Envar("SYSLOG_PROTOCOL").String()
	subscriptionId     = kingpin.Flag("subscription-id", "Id for the subscription.").Default("firehose").Envar("FIREHOSE_SUBSCRIPTION_ID").String()
	clientID           = kingpin.Flag("client-id", "Client ID.").Envar("FIREHOSE_CLIENT_ID").Required().String()
	clientSecret       = kingpin.Flag("client-secret", "Client secret.").Envar("FIREHOSE_CLIENT_SECRET").Required().String()
	skipSSLValidation  = kingpin.Flag("skip-ssl-validation", "Please don't").Default("false").Envar("SKIP_SSL_VALIDATION").Bool()
	keepAlive          = kingpin.Flag("fh-keep-alive", "Keep Alive duration for the firehose consumer").Default("25s").Envar("FH_KEEP_ALIVE").Duration()
	logEventTotals     = kingpin.Flag("log-event-totals", "Logs the counters for all selected events since nozzle was last started.").Default("false").Envar("LOG_EVENT_TOTALS").Bool()
	logEventTotalsTime = kingpin.Flag("log-event-totals-time", "How frequently the event totals are calculated (in sec).").Default("30s").Envar("LOG_EVENT_TOTALS_TIME").Duration()
	wantedEvents       = kingpin.Flag("events", fmt.Sprintf("Comma separated list of events you would like. Valid options are %s", eventRouting.GetListAuthorizedEventEvents())).Default("LogMessage").Envar("EVENTS").String()
	boltDatabasePath   = kingpin.Flag("boltdb-path", "Bolt Database path ").Default("my.db").Envar("BOLTDB_PATH").String()
	tickerTime         = kingpin.Flag("cc-pull-time", "CloudController Polling time in sec").Default("60s").Envar("CF_PULL_TIME").Duration()
	extraFields        = kingpin.Flag("extra-fields", "Extra fields you want to annotate your events with, example: '--extra-fields=env:dev,something:other ").Default("").Envar("EXTRA_FIELDS").String()
	modeProf           = kingpin.Flag("mode-prof", "Enable profiling mode, one of [cpu, mem, block]").Default("").Envar("MODE_PROF").String()
	pathProf           = kingpin.Flag("path-prof", "Set the Path to write profiling file").Default("").Envar("PATH_PROF").String()
	logFormatterType   = kingpin.Flag("log-formatter-type", "Log formatter type to use. Valid options are text, json. If none provided, defaults to json.").Envar("LOG_FORMATTER_TYPE").String()
	certPath           = kingpin.Flag("cert-pem-syslog", "Certificate Pem file").Envar("CERT_PEM").Default("").String()
	ignoreMissingApps  = kingpin.Flag("ignore-missing-apps", "Enable throttling on cache lookup for missing apps").Envar("IGNORE_MISSING_APPS").Default("false").Bool()
	missingAppsTtl     = kingpin.Flag("missing-apps-ttl", "Ticker time for clearing missing apps bucket").Envar("MISSING_APPS_TTL").Default("1h").Duration()
)

var (
	version = "0.0.0"
)

func main() {
	kingpin.Version(version)
	kingpin.Parse()

	//Setup Logging
	loggingClient := logging.NewLogging(*syslogServer, *syslogProtocol, *logFormatterType, *certPath, *debug)
	logging.LogStd(fmt.Sprintf("Starting firehose-to-syslog %s ", version), true)

	if *modeProf != "" {
		switch *modeProf {
		case "cpu":
			defer profile.Start(profile.CPUProfile, profile.ProfilePath(*pathProf)).Stop()
		case "mem":
			defer profile.Start(profile.MemProfile, profile.ProfilePath(*pathProf)).Stop()
		case "block":
			defer profile.Start(profile.BlockProfile, profile.ProfilePath(*pathProf)).Stop()
		default:
			// do nothing
		}
	}

	c := cfclient.Config{
		ApiAddress:        *apiEndpoint,
		ClientID:          *clientID,
		ClientSecret:      *clientSecret,
		SkipSslValidation: *skipSSLValidation,
		UserAgent:         "firehose-to-syslog/" + version,
	}
	cfClient, err := cfclient.NewClient(&c)
	if err != nil {
		log.Fatal("New Client: ", err)
		os.Exit(1)

	}
	if len(*dopplerEndpoint) > 0 {
		cfClient.Endpoint.DopplerEndpoint = *dopplerEndpoint
	}
	fmt.Println(cfClient.Endpoint.DopplerEndpoint)
	logging.LogStd(fmt.Sprintf("Using %s as doppler endpoint", cfClient.Endpoint.DopplerEndpoint), true)

	//Creating Caching
	var cachingClient caching.Caching
	if caching.IsNeeded(*wantedEvents) {
		config := &caching.CachingBoltConfig{
			Path: *boltDatabasePath,
			IgnoreMissingApps: *ignoreMissingApps,
			MissingAppsTTL: *missingAppsTtl,
			CacheInvalidateTTL:*tickerTime,
		}
		cachingClient, err = caching.NewCachingBolt(cfClient, config)
		if err != nil {
			log.Fatal("Failed to create boltdb cache", err)
		}
	} else {
		cachingClient = caching.NewCachingEmpty()
	}

	//Creating Events
	events := eventRouting.NewEventRouting(cachingClient, loggingClient)
	err = events.SetupEventRouting(*wantedEvents)
	if err != nil {
		log.Fatal("Error setting up event routing: ", err)
		os.Exit(1)

	}

	//Set extrafields if needed
	events.SetExtraFields(*extraFields)

	//Enable LogsTotalevent
	if *logEventTotals {
		logging.LogStd("Logging total events", true)
		events.LogEventTotals(*logEventTotalsTime)
	}

	if err := cachingClient.Open(); err != nil {
		log.Fatal("Error open cache: ", err)
	}

	uaaRefresher, err := uaatokenrefresher.NewUAATokenRefresher(
		cfClient.Endpoint.AuthEndpoint,
		*clientID,
		*clientSecret,
		*skipSSLValidation,
	)

	if err != nil {
		logging.LogError(fmt.Sprint("Failed connecting to Get token from UAA..", err), "")
	}

	firehoseConfig := &firehoseclient.FirehoseConfig{
		TrafficControllerURL:   cfClient.Endpoint.DopplerEndpoint,
		InsecureSSLSkipVerify:  *skipSSLValidation,
		IdleTimeoutSeconds:     *keepAlive,
		FirehoseSubscriptionID: *subscriptionId,
	}

	if loggingClient.Connect() || *debug {

		logging.LogStd("Connected to Syslog Server! Connecting to Firehose...", true)
		firehoseClient := firehoseclient.NewFirehoseNozzle(uaaRefresher, events, firehoseConfig)
		err = firehoseClient.Start()
		if err != nil {
			logging.LogError("Failed connecting to Firehose...Please check settings and try again!", "")

		} else {
			logging.LogStd("Firehose Subscription Succesfull! Routing events...", true)
		}

	} else {
		logging.LogError("Failed connecting to the Fluentd Server...Please check settings and try again!", "")
	}

	defer cachingClient.Close()
}
