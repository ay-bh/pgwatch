package main

import (
	"container/list"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cybertec-postgresql/pgwatch3/config"
	"github.com/cybertec-postgresql/pgwatch3/log"
	"github.com/cybertec-postgresql/pgwatch3/psutil"
	"github.com/cybertec-postgresql/pgwatch3/webserver"

	"github.com/coreos/go-systemd/daemon"
	"github.com/marpaia/graphite-golang"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/pbkdf2"
	"gopkg.in/yaml.v2"
)

type MonitoredDatabase struct {
	DBUniqueName         string `yaml:"unique_name"`
	DBUniqueNameOrig     string // to preserve belonging to a specific instance for continuous modes where DBUniqueName will be dynamic
	Group                string
	Host                 string
	Port                 string
	DBName               string
	User                 string
	Password             string
	PasswordType         string `yaml:"password_type"`
	LibPQConnStr         string `yaml:"libpq_conn_str"`
	SslMode              string
	SslRootCAPath        string             `yaml:"sslrootcert"`
	SslClientCertPath    string             `yaml:"sslcert"`
	SslClientKeyPath     string             `yaml:"sslkey"`
	Metrics              map[string]float64 `yaml:"custom_metrics"`
	MetricsStandby       map[string]float64 `yaml:"custom_metrics_standby"`
	StmtTimeout          int64              `yaml:"stmt_timeout"`
	DBType               string
	DBNameIncludePattern string            `yaml:"dbname_include_pattern"`
	DBNameExcludePattern string            `yaml:"dbname_exclude_pattern"`
	PresetMetrics        string            `yaml:"preset_metrics"`
	PresetMetricsStandby string            `yaml:"preset_metrics_standby"`
	IsSuperuser          bool              `yaml:"is_superuser"`
	IsEnabled            bool              `yaml:"is_enabled"`
	CustomTags           map[string]string `yaml:"custom_tags"` // ignored on graphite
	HostConfig           HostConfigAttrs   `yaml:"host_config"`
	OnlyIfMaster         bool              `yaml:"only_if_master"`
}

type HostConfigAttrs struct {
	DcsType                string   `yaml:"dcs_type"`
	DcsEndpoints           []string `yaml:"dcs_endpoints"`
	Scope                  string
	Namespace              string
	Username               string
	Password               string
	CAFile                 string                             `yaml:"ca_file"`
	CertFile               string                             `yaml:"cert_file"`
	KeyFile                string                             `yaml:"key_file"`
	LogsGlobPath           string                             `yaml:"logs_glob_path"`   // default $data_directory / $log_directory / *.csvlog
	LogsMatchRegex         string                             `yaml:"logs_match_regex"` // default is for CSVLOG format. needs to capture following named groups: log_time, user_name, database_name and error_severity
	PerMetricDisabledTimes []HostConfigPerMetricDisabledTimes `yaml:"per_metric_disabled_intervals"`
}

type HostConfigPerMetricDisabledTimes struct { // metric gathering override per host / metric / time
	Metrics       []string `yaml:"metrics"`
	DisabledTimes []string `yaml:"disabled_times"`
	DisabledDays  string   `yaml:"disabled_days"`
}

type PresetConfig struct {
	Name        string
	Description string
	Metrics     map[string]float64
}

type MetricColumnAttrs struct {
	PrometheusGaugeColumns    []string `yaml:"prometheus_gauge_columns"`
	PrometheusIgnoredColumns  []string `yaml:"prometheus_ignored_columns"` // for cases where we don't want some columns to be exposed in Prom mode
	PrometheusAllGaugeColumns bool     `yaml:"prometheus_all_gauge_columns"`
}

type MetricAttrs struct {
	IsInstanceLevel           bool                 `yaml:"is_instance_level"`
	MetricStorageName         string               `yaml:"metric_storage_name"`
	ExtensionVersionOverrides []ExtensionOverrides `yaml:"extension_version_based_overrides"`
	IsPrivate                 bool                 `yaml:"is_private"`                // used only for extension overrides currently and ignored otherwise
	DisabledDays              string               `yaml:"disabled_days"`             // Cron style, 0 = Sunday. Ranges allowed: 0,2-4
	DisableTimes              []string             `yaml:"disabled_times"`            // "11:00-13:00"
	StatementTimeoutSeconds   int64                `yaml:"statement_timeout_seconds"` // overrides per monitored DB settings
}

type MetricVersionProperties struct {
	SQL                  string
	SQLSU                string
	MasterOnly           bool
	StandbyOnly          bool
	ColumnAttrs          MetricColumnAttrs // Prometheus Metric Type (Counter is default) and ignore list
	MetricAttrs          MetricAttrs
	CallsHelperFunctions bool
}

type ControlMessage struct {
	Action string // START, STOP, PAUSE
	Config map[string]float64
}

type MetricFetchMessage struct {
	DBUniqueName        string
	DBUniqueNameOrig    string
	MetricName          string
	DBType              string
	Interval            time.Duration
	CreatedOn           time.Time
	StmtTimeoutOverride int64
}

type MetricEntry map[string]any
type MetricData []map[string]any

type MetricStoreMessage struct {
	DBUniqueName            string
	DBType                  string
	MetricName              string
	CustomTags              map[string]string
	Data                    MetricData
	MetricDefinitionDetails MetricVersionProperties
	RealDbname              string
	SystemIdentifier        string
}

type MetricStoreMessagePostgres struct {
	Time    time.Time
	DBName  string
	Metric  string
	Data    map[string]any
	TagData map[string]any
}

type ChangeDetectionResults struct { // for passing around DDL/index/config change detection results
	Created int
	Altered int
	Dropped int
}

type DBVersionMapEntry struct {
	LastCheckedOn    time.Time
	IsInRecovery     bool
	Version          decimal.Decimal
	VersionStr       string
	RealDbname       string
	SystemIdentifier string
	IsSuperuser      bool // if true and no helpers are installed, use superuser SQL version of metric if available
	Extensions       map[string]decimal.Decimal
	ExecEnv          string
	ApproxDBSizeB    int64
}

type ExistingPartitionInfo struct {
	StartTime time.Time
	EndTime   time.Time
}

type ExtensionOverrides struct {
	TargetMetric              string          `yaml:"target_metric"`
	ExpectedExtensionVersions []ExtensionInfo `yaml:"expected_extension_versions"`
}

type ExtensionInfo struct {
	ExtName       string          `yaml:"ext_name"`
	ExtMinVersion decimal.Decimal `yaml:"ext_min_version"`
}

const (
	epochColumnName             string = "epoch_ns" // this column (epoch in nanoseconds) is expected in every metric query
	tagPrefix                   string = "tag_"
	metricDefinitionRefreshTime int64  = 120 // min time before checking for new/changed metric definitions
	graphiteMetricsPrefix       string = "pgwatch3"
	persistQueueMaxSize                = 10000 // storage queue max elements. when reaching the limit, older metrics will be dropped.
)

// actual requirements depend a lot of metric type and nr. of obects in schemas,
// but 100k should be enough for 24h, assuming 5 hosts monitored with "exhaustive" preset config. this would also require ~2 GB RAM per one Influx host
const (
	datastoreGraphite         = "graphite"
	datastoreJSON             = "json"
	datastorePostgres         = "postgres"
	datastorePrometheus       = "prometheus"
	presetConfigYAMLFile      = "preset-configs.yaml"
	fileBasedMetricHelpersDir = "00_helpers"
	pgConnRecycleSeconds      = 1800       // applies for monitored nodes
	applicationName           = "pgwatch3" // will be set on all opened PG connections for informative purposes
	gathererStatusStart       = "START"
	gathererStatusStop        = "STOP"
	metricdbIdent             = "metricDb"
	configdbIdent             = "configDb"
	contextPrometheusScrape   = "prometheus-scrape"
	dcsTypeEtcd               = "etcd"
	dcsTypeZookeeper          = "zookeeper"
	dcsTypeConsul             = "consul"

	monitoredDbsDatastoreSyncIntervalSeconds = 600              // write actively monitored DBs listing to metrics store after so many seconds
	monitoredDbsDatastoreSyncMetricName      = "configured_dbs" // FYI - for Postgres datastore there's also the admin.all_unique_dbnames table with all recent DB unique names with some metric data
	recoPrefix                               = "reco_"          // special handling for metrics with such prefix, data stored in RECO_METRIC_NAME
	recoMetricName                           = "recommendations"
	specialMetricChangeEvents                = "change_events"
	specialMetricServerLogEventCounts        = "server_log_event_counts"
	specialMetricPgbouncer                   = "^pgbouncer_(stats|pools)$"
	specialMetricPgpoolStats                 = "pgpool_stats"
	specialMetricInstanceUp                  = "instance_up"
	specialMetricDbSize                      = "db_size"     // can be transparently switched to db_size_approx on instances with very slow FS access (Azure Single Server)
	specialMetricTableStats                  = "table_stats" // can be transparently switched to table_stats_approx on instances with very slow FS (Azure Single Server)
	metricCPULoad                            = "cpu_load"
	metricPsutilCPU                          = "psutil_cpu"
	metricPsutilDisk                         = "psutil_disk"
	metricPsutilDiskIoTotal                  = "psutil_disk_io_total"
	metricPsutilMem                          = "psutil_mem"
	defaultMetricsDefinitionPathPkg          = "/etc/pgwatch3/metrics" // prebuilt packages / Docker default location
	defaultMetricsDefinitionPathDocker       = "/pgwatch3/metrics"     // prebuilt packages / Docker default location
	dbSizeCachingInterval                    = 30 * time.Minute
	dbMetricJoinStr                          = "¤¤¤" // just some unlikely string for a DB name to avoid using maps of maps for DB+metric data
	execEnvUnknown                           = "UNKNOWN"
	execEnvAzureSingle                       = "AZURE_SINGLE"
	execEnvAzureFlexible                     = "AZURE_FLEXIBLE"
	execEnvGoogle                            = "GOOGLE"
)

var dbTypeMap = map[string]bool{config.DbTypePg: true, config.DbTypePgCont: true, config.DbTypeBouncer: true, config.DbTypePatroni: true, config.DbTypePatroniCont: true, config.DbTypePgPOOL: true, config.DbTypePatroniNamespaceDiscovery: true}
var dbTypes = []string{config.DbTypePg, config.DbTypePgCont, config.DbTypeBouncer, config.DbTypePatroni, config.DbTypePatroniCont, config.DbTypePatroniNamespaceDiscovery} // used for informational purposes
var specialMetrics = map[string]bool{recoMetricName: true, specialMetricChangeEvents: true, specialMetricServerLogEventCounts: true}
var directlyFetchableOSMetrics = map[string]bool{metricPsutilCPU: true, metricPsutilDisk: true, metricPsutilDiskIoTotal: true, metricPsutilMem: true, metricCPULoad: true}
var graphiteConnection *graphite.Graphite
var graphiteHost string
var graphitePort int
var metricDefinitionMap map[string]map[decimal.Decimal]MetricVersionProperties
var metricDefMapLock = sync.RWMutex{}
var hostMetricIntervalMap = make(map[string]float64) // [db1_metric] = 30
var dbPgVersionMap = make(map[string]DBVersionMapEntry)
var dbPgVersionMapLock = sync.RWMutex{}
var dbGetPgVersionMapLock = make(map[string]*sync.RWMutex) // synchronize initial PG version detection to 1 instance for each defined host
var monitoredDbCache map[string]MonitoredDatabase
var monitoredDbCacheLock sync.RWMutex

var monitoredDbConnCacheLock = sync.RWMutex{}
var lastSQLFetchError sync.Map
var influxHostCount = 1
var influxConnectStrings [2]string // Max. 2 Influx metrics stores currently supported

// secondary Influx meant for HA or Grafana load balancing for 100+ instances with lots of alerts
var fileBasedMetrics = false
var presetMetricDefMap map[string]map[string]float64 // read from metrics folder in "file mode"
// / internal statistics calculation
var lastSuccessfulDatastoreWriteTimeEpoch int64
var datastoreWriteFailuresCounter uint64
var datastoreWriteSuccessCounter uint64
var totalMetricFetchFailuresCounter uint64
var datastoreTotalWriteTimeMicroseconds uint64
var totalMetricsFetchedCounter uint64
var totalMetricsReusedFromCacheCounter uint64
var totalMetricsDroppedCounter uint64
var totalDatasetsFetchedCounter uint64
var metricPointsPerMinuteLast5MinAvg int64 = -1 // -1 means the summarization ticker has not yet run
var gathererStartTime = time.Now()
var partitionMapMetric = make(map[string]ExistingPartitionInfo)                  // metric = min/max bounds
var partitionMapMetricDbname = make(map[string]map[string]ExistingPartitionInfo) // metric[dbname = min/max bounds]
var testDataGenerationModeWG sync.WaitGroup
var PGDummyMetricTables = make(map[string]time.Time)
var PGDummyMetricTablesLock = sync.RWMutex{}
var PGSchemaType string
var failedInitialConnectHosts = make(map[string]bool) // hosts that couldn't be connected to even once
var forceRecreatePGMetricPartitions = false           // to signal override PG metrics storage cache
var lastMonitoredDBsUpdate time.Time
var instanceMetricCache = make(map[string](MetricData)) // [dbUnique+metric]lastly_fetched_data
var instanceMetricCacheLock = sync.RWMutex{}
var instanceMetricCacheTimestamp = make(map[string]time.Time) // [dbUnique+metric]last_fetch_time
var instanceMetricCacheTimestampLock = sync.RWMutex{}
var MinExtensionInfoAvailable, _ = decimal.NewFromString("9.1")
var regexIsAlpha = regexp.MustCompile("^[a-zA-Z]+$")
var rBouncerAndPgpoolVerMatch = regexp.MustCompile(`\d+\.+\d+`) // extract $major.minor from "4.1.2 (karasukiboshi)" or "PgBouncer 1.12.0"
var regexIsPgbouncerMetrics = regexp.MustCompile(specialMetricPgbouncer)
var unreachableDBsLock sync.RWMutex
var unreachableDB = make(map[string]time.Time)
var pgBouncerNumericCountersStartVersion decimal.Decimal // pgBouncer changed internal counters data type in v1.12

// Async Prom cache
var promAsyncMetricCache = make(map[string]map[string][]MetricStoreMessage) // [dbUnique][metric]lastly_fetched_data
var promAsyncMetricCacheLock = sync.RWMutex{}
var lastDBSizeMB = make(map[string]int64)
var lastDBSizeFetchTime = make(map[string]time.Time) // cached for DB_SIZE_CACHING_INTERVAL
var lastDBSizeCheckLock sync.RWMutex
var mainLoopInitialized int32 // 0/1

var prevLoopMonitoredDBs []MonitoredDatabase // to be able to detect DBs removed from config
var undersizedDBs = make(map[string]bool)    // DBs below the --min-db-size-mb limit, if set
var undersizedDBsLock = sync.RWMutex{}
var recoveryIgnoredDBs = make(map[string]bool) // DBs in recovery state and OnlyIfMaster specified in config
var recoveryIgnoredDBsLock = sync.RWMutex{}
var regexSQLHelperFunctionCalled = regexp.MustCompile(`(?si)^\s*(select|with).*\s+get_\w+\(\)[\s,$]+`) // SQL helpers expected to follow get_smth() naming
var metricNameRemaps = make(map[string]string)
var metricNameRemapLock = sync.RWMutex{}

var logger log.LoggerHookerIface

func RestoreSQLConnPoolLimitsForPreviouslyDormantDB(dbUnique string) {
	if !opts.UseConnPooling {
		return
	}
	monitoredDbConnCacheLock.Lock()
	defer monitoredDbConnCacheLock.Unlock()

	conn, ok := monitoredDbConnCache[dbUnique]
	if !ok || conn == nil {
		logger.Error("DB conn to re-instate pool limits not found, should not happen")
		return
	}

	logger.Debugf("[%s] Re-instating SQL connection pool max connections ...", dbUnique)

	conn.SetMaxIdleConns(opts.MaxParallelConnectionsPerDb)
	conn.SetMaxOpenConns(opts.MaxParallelConnectionsPerDb)

}

func InitPGVersionInfoFetchingLockIfNil(md MonitoredDatabase) {
	dbPgVersionMapLock.Lock()
	if _, ok := dbGetPgVersionMapLock[md.DBUniqueName]; !ok {
		dbGetPgVersionMapLock[md.DBUniqueName] = &sync.RWMutex{}
	}
	dbPgVersionMapLock.Unlock()
}

func GetMonitoredDatabasesFromConfigDB() ([]MonitoredDatabase, error) {
	monitoredDBs := make([]MonitoredDatabase, 0)
	activeHostData, err := GetAllActiveHostsFromConfigDB()
	groups := strings.Split(opts.Metric.Group, ",")
	skippedEntries := 0

	if err != nil {
		logger.Errorf("Failed to read monitoring config from DB: %s", err)
		return monitoredDBs, err
	}

	for _, row := range activeHostData {

		if len(opts.Metric.Group) > 0 { // filter out rows with non-matching groups
			matched := false
			for _, g := range groups {
				if row["md_group"].(string) == g {
					matched = true
					break
				}
			}
			if !matched {
				skippedEntries++
				continue
			}
		}
		if skippedEntries > 0 {
			logger.Infof("Filtered out %d config entries based on --groups input", skippedEntries)
		}

		metricConfig, err := jsonTextToMap(row["md_config"].(string))
		if err != nil {
			logger.Warningf("Cannot parse metrics JSON config for \"%s\": %v", row["md_unique_name"].(string), err)
			continue
		}
		metricConfigStandby := make(map[string]float64)
		if configStandby, ok := row["md_config_standby"]; ok {
			metricConfigStandby, err = jsonTextToMap(configStandby.(string))
			if err != nil {
				logger.Warningf("Cannot parse standby metrics JSON config for \"%s\". Ignoring standby config: %v", row["md_unique_name"].(string), err)
			}
		}
		customTags, err := jsonTextToStringMap(row["md_custom_tags"].(string))
		if err != nil {
			logger.Warningf("Cannot parse custom tags JSON for \"%s\". Ignoring custom tags. Error: %v", row["md_unique_name"].(string), err)
			customTags = nil
		}
		hostConfigAttrs := HostConfigAttrs{}
		err = yaml.Unmarshal([]byte(row["md_host_config"].(string)), &hostConfigAttrs)
		if err != nil {
			logger.Warningf("Cannot parse host config JSON for \"%s\". Ignoring host config. Error: %v", row["md_unique_name"].(string), err)
		}

		md := MonitoredDatabase{
			DBUniqueName:         row["md_unique_name"].(string),
			DBUniqueNameOrig:     row["md_unique_name"].(string),
			Host:                 row["md_hostname"].(string),
			Port:                 row["md_port"].(string),
			DBName:               row["md_dbname"].(string),
			User:                 row["md_user"].(string),
			IsSuperuser:          row["md_is_superuser"].(bool),
			Password:             row["md_password"].(string),
			PasswordType:         row["md_password_type"].(string),
			SslMode:              row["md_sslmode"].(string),
			SslRootCAPath:        row["md_root_ca_path"].(string),
			SslClientCertPath:    row["md_client_cert_path"].(string),
			SslClientKeyPath:     row["md_client_key_path"].(string),
			StmtTimeout:          row["md_statement_timeout_seconds"].(int64),
			Metrics:              metricConfig,
			MetricsStandby:       metricConfigStandby,
			DBType:               row["md_dbtype"].(string),
			DBNameIncludePattern: row["md_include_pattern"].(string),
			DBNameExcludePattern: row["md_exclude_pattern"].(string),
			Group:                row["md_group"].(string),
			HostConfig:           hostConfigAttrs,
			OnlyIfMaster:         row["md_only_if_master"].(bool),
			CustomTags:           customTags}

		if _, ok := dbTypeMap[md.DBType]; !ok {
			logger.Warningf("Ignoring host \"%s\" - unknown dbtype: %s. Expected one of: %+v", md.DBUniqueName, md.DBType, dbTypes)
			continue
		}

		if md.PasswordType == "aes-gcm-256" && opts.AesGcmKeyphrase != "" {
			md.Password = decrypt(md.DBUniqueName, opts.AesGcmKeyphrase, md.Password)
		}

		if md.DBType == config.DbTypePgCont {
			resolved, err := ResolveDatabasesFromConfigEntry(md)
			if err != nil {
				logger.Errorf("Failed to resolve DBs for \"%s\": %s", md.DBUniqueName, err)
				if md.PasswordType == "aes-gcm-256" && opts.AesGcmKeyphrase == "" {
					logger.Errorf("No decryption key set. Use the --aes-gcm-keyphrase or --aes-gcm-keyphrase params to set")
				}
				continue
			}
			tempArr := make([]string, 0)
			for _, rdb := range resolved {
				monitoredDBs = append(monitoredDBs, rdb)
				tempArr = append(tempArr, rdb.DBName)
			}
			logger.Debugf("Resolved %d DBs with prefix \"%s\": [%s]", len(resolved), md.DBUniqueName, strings.Join(tempArr, ", "))
		} else if md.DBType == config.DbTypePatroni || md.DBType == config.DbTypePatroniCont || md.DBType == config.DbTypePatroniNamespaceDiscovery {
			resolved, err := ResolveDatabasesFromPatroni(md)
			if err != nil {
				logger.Errorf("Failed to resolve DBs for \"%s\": %s", md.DBUniqueName, err)
				continue
			}
			tempArr := make([]string, 0)
			for _, rdb := range resolved {
				monitoredDBs = append(monitoredDBs, rdb)
				tempArr = append(tempArr, rdb.DBName)
			}
			logger.Debugf("Resolved %d DBs with prefix \"%s\": [%s]", len(resolved), md.DBUniqueName, strings.Join(tempArr, ", "))
		} else {
			monitoredDBs = append(monitoredDBs, md)
		}
	}
	return monitoredDBs, err
}

func InitGraphiteConnection(host string, port int) {
	var err error
	logger.Debug("Connecting to Graphite...")
	graphiteConnection, err = graphite.NewGraphite(host, port)
	if err != nil {
		logger.Fatal("could not connect to Graphite:", err)
	}
	logger.Debug("OK")
}

func SendToGraphite(dbname, measurement string, data MetricData) error {
	if len(data) == 0 {
		logger.Warning("No data passed to SendToGraphite call")
		return nil
	}
	logger.Debugf("Writing %d rows to Graphite", len(data))

	metricBasePrefix := graphiteMetricsPrefix + "." + measurement + "." + dbname + "."
	metrics := make([]graphite.Metric, 0, len(data)*len(data[0]))

	for _, dr := range data {
		var epochS int64

		// we loop over columns the first time just to find the timestamp
		for k, v := range dr {
			if v == nil || v == "" {
				continue // not storing NULLs
			} else if k == epochColumnName {
				epochS = v.(int64) / 1e9
				break
			}
		}

		if epochS == 0 {
			logger.Warning("No timestamp_ns found, server time will be used. measurement:", measurement)
			epochS = time.Now().Unix()
		}

		for k, v := range dr {
			if v == nil || v == "" {
				continue // not storing NULLs
			}
			if k == epochColumnName {
				continue
			}
			var metric graphite.Metric

			if strings.HasPrefix(k, tagPrefix) { // ignore tags for Graphite
				metric.Name = metricBasePrefix + k[4:]
			} else {
				metric.Name = metricBasePrefix + k
			}
			switch t := v.(type) {
			case int:
				metric.Value = fmt.Sprintf("%d", v)
			case int32:
				metric.Value = fmt.Sprintf("%d", v)
			case int64:
				metric.Value = fmt.Sprintf("%d", v)
			case float64:
				metric.Value = fmt.Sprintf("%f", v)
			default:
				logger.Infof("Invalid (non-numeric) column type ignored: metric %s, column: %v, return type: %T", measurement, k, t)
				continue
			}
			metric.Timestamp = epochS
			metrics = append(metrics, metric)

		}
	} // dr

	logger.Debug("Sending", len(metrics), "metric points to Graphite...")
	t1 := time.Now()
	err := graphiteConnection.SendMetrics(metrics)
	diff := time.Since(t1)
	if err != nil {
		atomic.AddUint64(&datastoreWriteFailuresCounter, 1)
		logger.Error("could not send metric to Graphite:", err)
	} else {
		atomic.StoreInt64(&lastSuccessfulDatastoreWriteTimeEpoch, t1.Unix())
		atomic.AddUint64(&datastoreTotalWriteTimeMicroseconds, uint64(diff.Microseconds()))
		atomic.AddUint64(&datastoreWriteSuccessCounter, 1)
		logger.Debug("Sent in ", diff.Microseconds(), "us")
	}

	return err
}

func GetMonitoredDatabaseByUniqueName(name string) (MonitoredDatabase, error) {
	monitoredDbCacheLock.RLock()
	defer monitoredDbCacheLock.RUnlock()
	_, exists := monitoredDbCache[name]
	if !exists {
		return MonitoredDatabase{}, errors.New("DBUnique not found")
	}
	return monitoredDbCache[name], nil
}

func UpdateMonitoredDBCache(data []MonitoredDatabase) {
	monitoredDbCacheNew := make(map[string]MonitoredDatabase)

	for _, row := range data {
		monitoredDbCacheNew[row.DBUniqueName] = row
	}

	monitoredDbCacheLock.Lock()
	monitoredDbCache = monitoredDbCacheNew
	monitoredDbCacheLock.Unlock()
}

func ProcessRetryQueue(dataSource, _, connIdent string, retryQueue *list.List, limit int) error {
	var err error
	iterationsDone := 0

	for retryQueue.Len() > 0 { // send over the whole re-try queue at once if connection works
		logger.Debug("Processing retry_queue", connIdent, ". Items in retry_queue: ", retryQueue.Len())
		msg := retryQueue.Back().Value.([]MetricStoreMessage)

		if dataSource == datastorePostgres {
			err = SendToPostgres(msg)
		} else if dataSource == datastoreGraphite {
			for _, m := range msg {
				err = SendToGraphite(m.DBUniqueName, m.MetricName, m.Data) // TODO add baching
				if err != nil {
					logger.Info("Reconnect to graphite")
					InitGraphiteConnection(graphiteHost, graphitePort)
				}
			}
		} else {
			logger.Fatal("Invalid datastore:", dataSource)
		}
		if err != nil {
			return err // still gone, retry later
		}
		retryQueue.Remove(retryQueue.Back())
		iterationsDone++
		if limit > 0 && limit == iterationsDone {
			return nil
		}
	}

	return nil
}

func MetricsBatcher(batchingMaxDelayMillis int64, bufferedStorageCh <-chan []MetricStoreMessage, storageCh chan<- []MetricStoreMessage) {
	if batchingMaxDelayMillis <= 0 {
		logger.Fatalf("Check --batching-delay-ms, zero/negative batching delay:", batchingMaxDelayMillis)
	}
	var datapointCounter int
	var maxBatchSize = 1000                // flush on maxBatchSize metric points or batchingMaxDelayMillis passed
	batch := make([]MetricStoreMessage, 0) // no size limit here as limited in persister already
	ticker := time.NewTicker(time.Millisecond * time.Duration(batchingMaxDelayMillis))

	for {
		select {
		case <-ticker.C:
			if len(batch) > 0 {
				flushed := make([]MetricStoreMessage, len(batch))
				copy(flushed, batch)
				logger.Debugf("Flushing %d metric datasets due to batching timeout", len(batch))
				storageCh <- flushed
				batch = make([]MetricStoreMessage, 0)
				datapointCounter = 0
			}
		case msg := <-bufferedStorageCh:
			for _, m := range msg { // in reality msg are sent by fetchers one by one though
				batch = append(batch, m)
				datapointCounter += len(m.Data)
				if datapointCounter > maxBatchSize { // flush. also set some last_sent_timestamp so that ticker would pass a round?
					flushed := make([]MetricStoreMessage, len(batch))
					copy(flushed, batch)
					logger.Debugf("Flushing %d metric datasets due to maxBatchSize limit of %d datapoints", len(batch), maxBatchSize)
					storageCh <- flushed
					batch = make([]MetricStoreMessage, 0)
					datapointCounter = 0
				}
			}
		}
	}
}

func WriteMetricsToJSONFile(msgArr []MetricStoreMessage, jsonPath string) error {
	if len(msgArr) == 0 {
		return nil
	}

	jsonOutFile, err := os.OpenFile(jsonPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		atomic.AddUint64(&datastoreWriteFailuresCounter, 1)
		return err
	}
	defer jsonOutFile.Close()

	logger.Infof("Writing %d metric sets to JSON file at \"%s\"...", len(msgArr), jsonPath)
	enc := json.NewEncoder(jsonOutFile)
	for _, msg := range msgArr {
		dataRow := map[string]any{"metric": msg.MetricName, "data": msg.Data, "dbname": msg.DBUniqueName, "custom_tags": msg.CustomTags}
		if opts.AddRealDbname && msg.RealDbname != "" {
			dataRow[opts.RealDbnameField] = msg.RealDbname
		}
		if opts.AddSystemIdentifier && msg.SystemIdentifier != "" {
			dataRow[opts.SystemIdentifierField] = msg.SystemIdentifier
		}
		err = enc.Encode(dataRow)
		if err != nil {
			atomic.AddUint64(&datastoreWriteFailuresCounter, 1)
			return err
		}
	}
	return nil
}

func MetricsPersister(dataStore string, storageCh <-chan []MetricStoreMessage) {
	var lastЕry = make([]time.Time, influxHostCount)         // if Influx errors out, don't retry before 10s
	var lastDropWarning = make([]time.Time, influxHostCount) // log metric points drops every 10s to not overflow logs in case Influx is down for longer
	var retryQueues = make([]*list.List, influxHostCount)    // separate queues for all Influx hosts
	var inError = make([]bool, influxHostCount)
	var err error

	for i := 0; i < influxHostCount; i++ {
		retryQueues[i] = list.New()
	}

	for {
		select {
		case msgArr := <-storageCh:

			logger.Debug("Metric Storage Messages:", msgArr)

			for i, retryQueue := range retryQueues {

				retryQueueLength := retryQueue.Len()

				if retryQueueLength > 0 {
					if retryQueueLength == persistQueueMaxSize {
						droppedMsgs := retryQueue.Remove(retryQueue.Back())
						datasetsDropped := len(droppedMsgs.([]MetricStoreMessage))
						datapointsDropped := 0
						for _, msg := range droppedMsgs.([]MetricStoreMessage) {
							datapointsDropped += len(msg.Data)
						}
						atomic.AddUint64(&totalMetricsDroppedCounter, uint64(datapointsDropped))
						if lastDropWarning[i].IsZero() || lastDropWarning[i].Before(time.Now().Add(time.Second*-10)) {
							logger.Warningf("Dropped %d oldest data sets with %d data points from queue %d as PERSIST_QUEUE_MAX_SIZE = %d exceeded",
								datasetsDropped, datapointsDropped, i, persistQueueMaxSize)
							lastDropWarning[i] = time.Now()
						}
					}
					retryQueue.PushFront(msgArr)
				} else {
					if dataStore == datastorePrometheus && opts.Metric.PrometheusAsyncMode {
						if len(msgArr) == 0 || len(msgArr[0].Data) == 0 { // no batching in async prom mode, so using 0 indexing ok
							continue
						}
						msg := msgArr[0]
						PromAsyncCacheAddMetricData(msg.DBUniqueName, msg.MetricName, msgArr)
						logger.Infof("[%s:%s] Added %d rows to Prom cache", msg.DBUniqueName, msg.MetricName, len(msg.Data))
					} else if dataStore == datastorePostgres {
						err = SendToPostgres(msgArr)
						if err != nil && strings.Contains(err.Error(), "does not exist") {
							// in case data was cleaned by user externally
							logger.Warning("re-initializing metric partition cache due to possible external data cleanup...")
							partitionMapMetric = make(map[string]ExistingPartitionInfo)
							partitionMapMetricDbname = make(map[string]map[string]ExistingPartitionInfo)
						}
					} else if dataStore == datastoreGraphite {
						for _, m := range msgArr {
							err = SendToGraphite(m.DBUniqueName, m.MetricName, m.Data) // TODO does Graphite library support batching?
							if err != nil {
								atomic.AddUint64(&datastoreWriteFailuresCounter, 1)
							}
						}
					} else if dataStore == datastoreJSON {
						err = WriteMetricsToJSONFile(msgArr, opts.Metric.JSONStorageFile)
					} else {
						logger.Fatal("Invalid datastore:", dataStore)
					}
					lastЕry[i] = time.Now()

					if err != nil {
						logger.Errorf("Failed to write into datastore %d: %s", i, err)
						inError[i] = true
						retryQueue.PushFront(msgArr)
					}
				}
			}
		default:
			for i, retryQueue := range retryQueues {
				if retryQueue.Len() > 0 && (!inError[i] || lastЕry[i].Before(time.Now().Add(time.Second*-10))) {
					err := ProcessRetryQueue(dataStore, influxConnectStrings[i], strconv.Itoa(i), retryQueue, 100)
					if err != nil {
						logger.Error("Error processing retry queue", i, ":", err)
						inError[i] = true
					} else {
						inError[i] = false
					}
					lastЕry[i] = time.Now()
				} else {
					time.Sleep(time.Millisecond * 100) // nothing in queue nor in channel
				}
			}
		}
	}
}

// Need to define a sort interface as Go doesn't have support for Numeric/Decimal
type Decimal []decimal.Decimal

func (a Decimal) Len() int           { return len(a) }
func (a Decimal) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a Decimal) Less(i, j int) bool { return a[i].LessThan(a[j]) }

// assumes upwards compatibility for versions
func GetMetricVersionProperties(metric string, vme DBVersionMapEntry, metricDefMap map[string]map[decimal.Decimal]MetricVersionProperties) (MetricVersionProperties, error) {
	var keys []decimal.Decimal
	var mdm map[string]map[decimal.Decimal]MetricVersionProperties

	if metricDefMap != nil {
		mdm = metricDefMap
	} else {
		metricDefMapLock.RLock()
		mdm = deepCopyMetricDefinitionMap(metricDefinitionMap) // copy of global cache
		metricDefMapLock.RUnlock()
	}

	_, ok := mdm[metric]
	if !ok || len(mdm[metric]) == 0 {
		logger.Debug("metric", metric, "not found")
		return MetricVersionProperties{}, errors.New("metric SQL not found")
	}

	for k := range mdm[metric] {
		keys = append(keys, k)
	}

	sort.Sort(Decimal(keys))

	var bestVer decimal.Decimal
	var minVer decimal.Decimal
	var found bool
	for _, ver := range keys {
		if vme.Version.GreaterThanOrEqual(ver) {
			bestVer = ver
			found = true
		}
		if minVer.IsZero() || ver.LessThan(minVer) {
			minVer = ver
		}
	}

	if !found {
		if vme.Version.LessThan(minVer) { // metric not yet available for given PG ver
			return MetricVersionProperties{}, fmt.Errorf("no suitable SQL found for metric \"%s\", server version \"%s\" too old. min defined SQL ver: %s", metric, vme.VersionStr, minVer.String())
		}
		return MetricVersionProperties{}, fmt.Errorf("no suitable SQL found for metric \"%s\", version \"%s\"", metric, vme.VersionStr)
	}

	ret := mdm[metric][bestVer]

	// check if SQL def. override defined for some specific extension version and replace the metric SQL-s if so
	if ret.MetricAttrs.ExtensionVersionOverrides != nil && len(ret.MetricAttrs.ExtensionVersionOverrides) > 0 {
		if vme.Extensions != nil && len(vme.Extensions) > 0 {
			logger.Debugf("[%s] extension version based override request found: %+v", metric, ret.MetricAttrs.ExtensionVersionOverrides)
			for _, extOverride := range ret.MetricAttrs.ExtensionVersionOverrides {
				var matching = true
				for _, extVer := range extOverride.ExpectedExtensionVersions { // "natural" sorting of metric definition assumed
					installedExtVer, ok := vme.Extensions[extVer.ExtName]
					if !ok || !installedExtVer.GreaterThanOrEqual(extVer.ExtMinVersion) {
						matching = false
					}
				}
				if matching { // all defined extensions / versions (if many) need to match
					_, ok := mdm[extOverride.TargetMetric]
					if !ok {
						logger.Warningf("extension based override metric not found for metric %s. substitute metric name: %s", metric, extOverride.TargetMetric)
						continue
					}
					mvp, err := GetMetricVersionProperties(extOverride.TargetMetric, vme, mdm)
					if err != nil {
						logger.Warningf("undefined extension based override for metric %s, substitute metric name: %s, version: %s not found", metric, extOverride.TargetMetric, bestVer)
						continue
					}
					logger.Debugf("overriding metric %s based on the extension_version_based_overrides metric attribute with %s:%s", metric, extOverride.TargetMetric, bestVer)
					if mvp.SQL != "" {
						ret.SQL = mvp.SQL
					}
					if mvp.SQLSU != "" {
						ret.SQLSU = mvp.SQLSU
					}
				}
			}
		}
	}
	return ret, nil
}

func GetAllRecoMetricsForVersion(vme DBVersionMapEntry) map[string]MetricVersionProperties {
	mvpMap := make(map[string]MetricVersionProperties)
	metricDefMapLock.RLock()
	defer metricDefMapLock.RUnlock()
	for m := range metricDefinitionMap {
		if strings.HasPrefix(m, recoPrefix) {
			mvp, err := GetMetricVersionProperties(m, vme, metricDefinitionMap)
			if err != nil {
				logger.Warningf("Could not get SQL definition for metric \"%s\", PG %s", m, vme.VersionStr)
			} else if !mvp.MetricAttrs.IsPrivate {
				mvpMap[m] = mvp
			}
		}
	}
	return mvpMap
}

func GetRecommendations(dbUnique string, vme DBVersionMapEntry) (MetricData, time.Duration, error) {
	retData := make(MetricData, 0)
	var totalDuration time.Duration
	startTimeEpochNs := time.Now().UnixNano()

	recoMetrics := GetAllRecoMetricsForVersion(vme)
	logger.Debugf("Processing %d recommendation metrics for \"%s\"", len(recoMetrics), dbUnique)

	for m, mvp := range recoMetrics {
		data, duration, err := DBExecReadByDbUniqueName(dbUnique, m, mvp.MetricAttrs.StatementTimeoutSeconds, mvp.SQL)
		totalDuration += duration
		if err != nil {
			if strings.Contains(err.Error(), "does not exist") { // some more exotic extensions missing is expected, don't pollute the error log
				logger.Infof("[%s:%s] Could not execute recommendations SQL: %v", dbUnique, m, err)
			} else {
				logger.Errorf("[%s:%s] Could not execute recommendations SQL: %v", dbUnique, m, err)
			}
			continue
		}
		for _, d := range data {
			d[epochColumnName] = startTimeEpochNs
			d["major_ver"] = PgVersionDecimalToMajorVerFloat(dbUnique, vme.Version)
			retData = append(retData, d)
		}
	}
	if len(retData) == 0 { // insert a dummy entry minimally so that Grafana can show at least a dropdown
		dummy := make(MetricEntry)
		dummy["tag_reco_topic"] = "dummy"
		dummy["tag_object_name"] = "-"
		dummy["recommendation"] = "no recommendations"
		dummy[epochColumnName] = startTimeEpochNs
		dummy["major_ver"] = PgVersionDecimalToMajorVerFloat(dbUnique, vme.Version)
		retData = append(retData, dummy)
	}
	return retData, totalDuration, nil
}

func PgVersionDecimalToMajorVerFloat(_ string, pgVer decimal.Decimal) float64 {
	verFloat, _ := pgVer.Float64()
	if verFloat >= 10 {
		return math.Floor(verFloat)
	}
	return verFloat
}

func FilterPgbouncerData(data MetricData, databaseToKeep string, vme DBVersionMapEntry) MetricData {
	filteredData := make(MetricData, 0)

	for _, dr := range data {
		//log.Debugf("bouncer dr: %+v", dr)
		if _, ok := dr["database"]; !ok {
			logger.Warning("Expected 'database' key not found from pgbouncer_stats, not storing data")
			continue
		}
		if (len(databaseToKeep) > 0 && dr["database"] != databaseToKeep) || dr["database"] == "pgbouncer" { // always ignore the internal 'pgbouncer' DB
			logger.Debugf("Skipping bouncer stats for pool entry %v as not the specified DBName of %s", dr["database"], databaseToKeep)
			continue // and all others also if a DB / pool name was specified in config
		}

		dr["tag_database"] = dr["database"] // support multiple databases / pools via tags if DbName left empty
		delete(dr, "database")              // remove the original pool name

		if vme.Version.GreaterThanOrEqual(pgBouncerNumericCountersStartVersion) { // v1.12 counters are of type numeric instead of int64
			for k, v := range dr {
				if k == "tag_database" {
					continue
				}
				decimalCounter, err := decimal.NewFromString(string(v.([]uint8)))
				if err != nil {
					logger.Errorf("Could not parse \"%+v\" to Decimal: %s", string(v.([]uint8)), err)
					return filteredData
				}
				dr[k] = decimalCounter.IntPart() // technically could cause overflow...but highly unlikely for 2^63
			}
		}
		filteredData = append(filteredData, dr)
	}

	return filteredData
}

func FetchMetrics(msg MetricFetchMessage, hostState map[string]map[string]string, storageCh chan<- []MetricStoreMessage, context string) ([]MetricStoreMessage, error) {
	var vme DBVersionMapEntry
	var dbpgVersion decimal.Decimal
	var err, firstErr error
	var sql string
	var retryWithSuperuserSQL = true
	var data, cachedData MetricData
	var duration time.Duration
	var md MonitoredDatabase
	var fromCache, isCacheable bool

	vme, err = DBGetPGVersion(msg.DBUniqueName, msg.DBType, false)
	if err != nil {
		logger.Error("failed to fetch pg version for ", msg.DBUniqueName, msg.MetricName, err)
		return nil, err
	}
	if msg.MetricName == specialMetricDbSize || msg.MetricName == specialMetricTableStats {
		if vme.ExecEnv == execEnvAzureSingle && vme.ApproxDBSizeB > 1e12 { // 1TB
			subsMetricName := msg.MetricName + "_approx"
			mvpApprox, err := GetMetricVersionProperties(subsMetricName, vme, nil)
			if err == nil && mvpApprox.MetricAttrs.MetricStorageName == msg.MetricName {
				logger.Infof("[%s:%s] Transparently swapping metric to %s due to hard-coded rules...", msg.DBUniqueName, msg.MetricName, subsMetricName)
				msg.MetricName = subsMetricName
			}
		}
	}
	dbpgVersion = vme.Version

	if msg.DBType == config.DbTypeBouncer {
		dbpgVersion = decimal.Decimal{} // version is 0.0 for all pgbouncer sql per convention
	}

	mvp, err := GetMetricVersionProperties(msg.MetricName, vme, nil)
	if err != nil && msg.MetricName != recoMetricName {
		epoch, ok := lastSQLFetchError.Load(msg.MetricName + dbMetricJoinStr + dbpgVersion.String())
		if !ok || ((time.Now().Unix() - epoch.(int64)) > 3600) { // complain only 1x per hour
			logger.Infof("Failed to get SQL for metric '%s', version '%s': %v", msg.MetricName, vme.VersionStr, err)
			lastSQLFetchError.Store(msg.MetricName+dbMetricJoinStr+dbpgVersion.String(), time.Now().Unix())
		}
		if strings.Contains(err.Error(), "too old") {
			return nil, nil
		}
		return nil, err
	}

	isCacheable = IsCacheableMetric(msg, mvp)
	if isCacheable && opts.InstanceLevelCacheMaxSeconds > 0 && msg.Interval.Seconds() > float64(opts.InstanceLevelCacheMaxSeconds) {
		cachedData = GetFromInstanceCacheIfNotOlderThanSeconds(msg, opts.InstanceLevelCacheMaxSeconds)
		if len(cachedData) > 0 {
			fromCache = true
			goto send_to_storageChannel
		}
	}

retry_with_superuser_sql: // if 1st fetch with normal SQL fails, try with SU SQL if it's defined

	sql = mvp.SQL

	if opts.Metric.NoHelperFunctions && mvp.CallsHelperFunctions && mvp.SQLSU != "" {
		logger.Debugf("[%s:%s] Using SU SQL instead of normal one due to --no-helper-functions input", msg.DBUniqueName, msg.MetricName)
		sql = mvp.SQLSU
		retryWithSuperuserSQL = false
	}

	if (vme.IsSuperuser || (retryWithSuperuserSQL && firstErr != nil)) && mvp.SQLSU != "" {
		sql = mvp.SQLSU
		retryWithSuperuserSQL = false
	}
	if sql == "" && !(msg.MetricName == specialMetricChangeEvents || msg.MetricName == recoMetricName) {
		// let's ignore dummy SQL-s
		logger.Debugf("[%s:%s] Ignoring fetch message - got an empty/dummy SQL string", msg.DBUniqueName, msg.MetricName)
		return nil, nil
	}

	if (mvp.MasterOnly && vme.IsInRecovery) || (mvp.StandbyOnly && !vme.IsInRecovery) {
		logger.Debugf("[%s:%s] Skipping fetching of  as server not in wanted state (IsInRecovery=%v)", msg.DBUniqueName, msg.MetricName, vme.IsInRecovery)
		return nil, nil
	}

	if msg.MetricName == specialMetricChangeEvents && context != contextPrometheusScrape { // special handling, multiple queries + stateful
		CheckForPGObjectChangesAndStore(msg.DBUniqueName, vme, storageCh, hostState) // TODO no hostState for Prometheus currently
	} else if msg.MetricName == recoMetricName && context != contextPrometheusScrape {
		data, _, _ = GetRecommendations(msg.DBUniqueName, vme)
	} else if msg.DBType == config.DbTypePgPOOL {
		data, _, _ = FetchMetricsPgpool(msg, vme, mvp)
	} else {
		data, duration, err = DBExecReadByDbUniqueName(msg.DBUniqueName, msg.MetricName, mvp.MetricAttrs.StatementTimeoutSeconds, sql)

		if err != nil {
			// let's soften errors to "info" from functions that expect the server to be a primary to reduce noise
			if strings.Contains(err.Error(), "recovery is in progress") {
				dbPgVersionMapLock.RLock()
				ver := dbPgVersionMap[msg.DBUniqueName]
				dbPgVersionMapLock.RUnlock()
				if ver.IsInRecovery {
					logger.Debugf("[%s:%s] failed to fetch metrics: %s", msg.DBUniqueName, msg.MetricName, err)
					return nil, err
				}
			}

			if msg.MetricName == specialMetricInstanceUp {
				logger.Debugf("[%s:%s] failed to fetch metrics. marking instance as not up: %s", msg.DBUniqueName, msg.MetricName, err)
				data = make(MetricData, 1)
				data[0] = MetricEntry{"epoch_ns": time.Now().UnixNano(), "is_up": 0} // NB! should be updated if the "instance_up" metric definition is changed
				goto send_to_storageChannel
			}

			if strings.Contains(err.Error(), "connection refused") {
				SetDBUnreachableState(msg.DBUniqueName)
			}

			if retryWithSuperuserSQL && mvp.SQLSU != "" {
				firstErr = err
				logger.Infof("[%s:%s] Normal fetch failed, re-trying to fetch with SU SQL", msg.DBUniqueName, msg.MetricName)
				goto retry_with_superuser_sql
			}
			if firstErr != nil {
				logger.Infof("[%s:%s] failed to fetch metrics also with SU SQL so initial error will be returned. Current err: %s", msg.DBUniqueName, msg.MetricName, err)
				return nil, firstErr // returning the initial error
			}
			logger.Infof("[%s:%s] failed to fetch metrics: %s", msg.DBUniqueName, msg.MetricName, err)

			return nil, err
		}
		md, err = GetMonitoredDatabaseByUniqueName(msg.DBUniqueName)
		if err != nil {
			logger.Errorf("[%s:%s] could not get monitored DB details", msg.DBUniqueName, err)
			return nil, err
		}

		logger.Infof("[%s:%s] fetched %d rows in %.1f ms", msg.DBUniqueName, msg.MetricName, len(data), float64(duration.Nanoseconds())/1000000)
		if regexIsPgbouncerMetrics.MatchString(msg.MetricName) { // clean unwanted pgbouncer pool stats here as not possible in SQL
			data = FilterPgbouncerData(data, md.DBName, vme)
		}

		ClearDBUnreachableStateIfAny(msg.DBUniqueName)

	}

	if isCacheable && opts.InstanceLevelCacheMaxSeconds > 0 && msg.Interval.Seconds() > float64(opts.InstanceLevelCacheMaxSeconds) {
		PutToInstanceCache(msg, data)
	}

send_to_storageChannel:

	if (opts.AddRealDbname || opts.AddSystemIdentifier) && msg.DBType == config.DbTypePg {
		dbPgVersionMapLock.RLock()
		ver := dbPgVersionMap[msg.DBUniqueName]
		dbPgVersionMapLock.RUnlock()
		data = AddDbnameSysinfoIfNotExistsToQueryResultData(msg, data, ver)
	}

	if mvp.MetricAttrs.MetricStorageName != "" {
		logger.Debugf("[%s] rerouting metric %s data to %s based on metric attributes", msg.DBUniqueName, msg.MetricName, mvp.MetricAttrs.MetricStorageName)
		msg.MetricName = mvp.MetricAttrs.MetricStorageName
	}
	if fromCache {
		md, err = GetMonitoredDatabaseByUniqueName(msg.DBUniqueName)
		if err != nil {
			logger.Errorf("[%s:%s] could not get monitored DB details", msg.DBUniqueName, err)
			return nil, err
		}
		logger.Infof("[%s:%s] loaded %d rows from the instance cache", msg.DBUniqueName, msg.MetricName, len(cachedData))
		atomic.AddUint64(&totalMetricsReusedFromCacheCounter, uint64(len(cachedData)))
		return []MetricStoreMessage{{DBUniqueName: msg.DBUniqueName, MetricName: msg.MetricName, Data: cachedData, CustomTags: md.CustomTags,
			MetricDefinitionDetails: mvp, RealDbname: vme.RealDbname, SystemIdentifier: vme.SystemIdentifier}}, nil
	}
	atomic.AddUint64(&totalMetricsFetchedCounter, uint64(len(data)))
	return []MetricStoreMessage{{DBUniqueName: msg.DBUniqueName, MetricName: msg.MetricName, Data: data, CustomTags: md.CustomTags,
		MetricDefinitionDetails: mvp, RealDbname: vme.RealDbname, SystemIdentifier: vme.SystemIdentifier}}, nil

}

func SetDBUnreachableState(dbUnique string) {
	unreachableDBsLock.Lock()
	unreachableDB[dbUnique] = time.Now()
	unreachableDBsLock.Unlock()
}

func ClearDBUnreachableStateIfAny(dbUnique string) {
	unreachableDBsLock.Lock()
	delete(unreachableDB, dbUnique)
	unreachableDBsLock.Unlock()
}

func PurgeMetricsFromPromAsyncCacheIfAny(dbUnique, metric string) {
	if opts.Metric.PrometheusAsyncMode {
		promAsyncMetricCacheLock.Lock()
		defer promAsyncMetricCacheLock.Unlock()

		if metric == "" {
			delete(promAsyncMetricCache, dbUnique) // whole host removed from config
		} else {
			delete(promAsyncMetricCache[dbUnique], metric)
		}
	}
}

func GetFromInstanceCacheIfNotOlderThanSeconds(msg MetricFetchMessage, maxAgeSeconds int64) MetricData {
	var clonedData MetricData
	instanceMetricCacheTimestampLock.RLock()
	instanceMetricTS, ok := instanceMetricCacheTimestamp[msg.DBUniqueNameOrig+msg.MetricName]
	instanceMetricCacheTimestampLock.RUnlock()
	if !ok {
		//log.Debugf("[%s:%s] no instance cache entry", msg.DBUniqueNameOrig, msg.MetricName)
		return nil
	}

	if time.Now().Unix()-instanceMetricTS.Unix() > maxAgeSeconds {
		//log.Debugf("[%s:%s] instance cache entry too old", msg.DBUniqueNameOrig, msg.MetricName)
		return nil
	}

	logger.Debugf("[%s:%s] reading metric data from instance cache of \"%s\"", msg.DBUniqueName, msg.MetricName, msg.DBUniqueNameOrig)
	instanceMetricCacheLock.RLock()
	instanceMetricData, ok := instanceMetricCache[msg.DBUniqueNameOrig+msg.MetricName]
	if !ok {
		instanceMetricCacheLock.RUnlock()
		return nil
	}
	clonedData = deepCopyMetricData(instanceMetricData)
	instanceMetricCacheLock.RUnlock()

	return clonedData
}

func PutToInstanceCache(msg MetricFetchMessage, data MetricData) {
	if len(data) == 0 {
		return
	}
	dataCopy := deepCopyMetricData(data)
	logger.Debugf("[%s:%s] filling instance cache", msg.DBUniqueNameOrig, msg.MetricName)
	instanceMetricCacheLock.Lock()
	instanceMetricCache[msg.DBUniqueNameOrig+msg.MetricName] = dataCopy
	instanceMetricCacheLock.Unlock()

	instanceMetricCacheTimestampLock.Lock()
	instanceMetricCacheTimestamp[msg.DBUniqueNameOrig+msg.MetricName] = time.Now()
	instanceMetricCacheTimestampLock.Unlock()
}

func IsCacheableMetric(msg MetricFetchMessage, mvp MetricVersionProperties) bool {
	if !(msg.DBType == config.DbTypePgCont || msg.DBType == config.DbTypePatroniCont) {
		return false
	}
	return mvp.MetricAttrs.IsInstanceLevel
}

func AddDbnameSysinfoIfNotExistsToQueryResultData(msg MetricFetchMessage, data MetricData, ver DBVersionMapEntry) MetricData {
	enrichedData := make(MetricData, 0)

	logger.Debugf("Enriching all rows of [%s:%s] with sysinfo (%s) / real dbname (%s) if set. ", msg.DBUniqueName, msg.MetricName, ver.SystemIdentifier, ver.RealDbname)
	for _, dr := range data {
		if opts.AddRealDbname && ver.RealDbname != "" {
			old, ok := dr[tagPrefix+opts.RealDbnameField]
			if !ok || old == "" {
				dr[tagPrefix+opts.RealDbnameField] = ver.RealDbname
			}
		}
		if opts.AddSystemIdentifier && ver.SystemIdentifier != "" {
			old, ok := dr[tagPrefix+opts.SystemIdentifierField]
			if !ok || old == "" {
				dr[tagPrefix+opts.SystemIdentifierField] = ver.SystemIdentifier
			}
		}
		enrichedData = append(enrichedData, dr)
	}
	return enrichedData
}

func StoreMetrics(metrics []MetricStoreMessage, storageCh chan<- []MetricStoreMessage) (int, error) {

	if len(metrics) > 0 {
		atomic.AddUint64(&totalDatasetsFetchedCounter, 1)
		storageCh <- metrics
		return len(metrics), nil
	}

	return 0, nil
}

func deepCopyMetricStoreMessages(metricStoreMessages []MetricStoreMessage) []MetricStoreMessage {
	newMsgs := make([]MetricStoreMessage, 0)
	for _, msm := range metricStoreMessages {
		dataNew := make(MetricData, 0)
		for _, dr := range msm.Data {
			drNew := make(map[string]any)
			for k, v := range dr {
				drNew[k] = v
			}
			dataNew = append(dataNew, drNew)
		}
		tagDataNew := make(map[string]string)
		for k, v := range msm.CustomTags {
			tagDataNew[k] = v
		}

		m := MetricStoreMessage{DBUniqueName: msm.DBUniqueName, MetricName: msm.MetricName, DBType: msm.DBType,
			Data: dataNew, CustomTags: tagDataNew}
		newMsgs = append(newMsgs, m)
	}
	return newMsgs
}

func deepCopyMetricData(data MetricData) MetricData {
	newData := make(MetricData, len(data))

	for i, dr := range data {
		newRow := make(map[string]any)
		for k, v := range dr {
			newRow[k] = v
		}
		newData[i] = newRow
	}

	return newData
}

func deepCopyMetricDefinitionMap(mdm map[string]map[decimal.Decimal]MetricVersionProperties) map[string]map[decimal.Decimal]MetricVersionProperties {
	newMdm := make(map[string]map[decimal.Decimal]MetricVersionProperties)

	for metric, verMap := range mdm {
		newMdm[metric] = make(map[decimal.Decimal]MetricVersionProperties)
		for ver, mvp := range verMap {
			newMdm[metric][ver] = mvp
		}
	}
	return newMdm
}

// ControlMessage notifies of shutdown + interval change
func MetricGathererLoop(dbUniqueName, dbUniqueNameOrig, dbType, metricName string, configMap map[string]float64, controlCh <-chan ControlMessage, storeCh chan<- []MetricStoreMessage) {
	config := configMap
	interval := config[metricName]
	ticker := time.NewTicker(time.Millisecond * time.Duration(interval*1000))
	hostState := make(map[string]map[string]string)
	var lastUptimeS int64 = -1 // used for "server restarted" event detection
	var lastErrorNotificationTime time.Time
	var vme DBVersionMapEntry
	var mvp MetricVersionProperties
	var err error
	failedFetches := 0
	metricNameForStorage := metricName
	lastDBVersionFetchTime := time.Unix(0, 0) // check DB ver. ev. 5 min
	var stmtTimeoutOverride int64

	if opts.TestdataDays != 0 {
		if metricName == specialMetricServerLogEventCounts || metricName == specialMetricChangeEvents {
			return
		}
		testDataGenerationModeWG.Add(1)
	}
	if opts.Metric.Datastore == datastorePostgres && opts.TestdataDays == 0 {
		if _, isSpecialMetric := specialMetrics[metricName]; !isSpecialMetric {
			vme, err := DBGetPGVersion(dbUniqueName, dbType, false)
			if err != nil {
				logger.Warningf("[%s][%s] Failed to determine possible re-routing name, Grafana dashboards with re-routed metrics might not show all hosts", dbUniqueName, metricName)
			} else {
				mvp, err := GetMetricVersionProperties(metricName, vme, nil)
				if err != nil && !strings.Contains(err.Error(), "too old") {
					logger.Warningf("[%s][%s] Failed to determine possible re-routing name, Grafana dashboards with re-routed metrics might not show all hosts", dbUniqueName, metricName)
				} else if mvp.MetricAttrs.MetricStorageName != "" {
					metricNameForStorage = mvp.MetricAttrs.MetricStorageName
				}
			}
		}

		err := AddDBUniqueMetricToListingTable(dbUniqueName, metricNameForStorage)
		if err != nil {
			logger.Errorf("Could not add newly found gatherer [%s:%s] to the 'all_distinct_dbname_metrics' listing table: %v", dbUniqueName, metricName, err)
		}

		EnsureMetricDummy(metricNameForStorage) // ensure that there is at least an empty top-level table not to get ugly Grafana notifications
	}

	if metricName == specialMetricServerLogEventCounts {
		logparseLoop(dbUniqueName, metricName, configMap, controlCh, storeCh) // no return
		return
	}

	for {
		if lastDBVersionFetchTime.Add(time.Minute * time.Duration(5)).Before(time.Now()) {
			vme, err = DBGetPGVersion(dbUniqueName, dbType, false) // in case of errors just ignore metric "disabled" time ranges
			if err != nil {
				lastDBVersionFetchTime = time.Now()
			}

			mvp, err = GetMetricVersionProperties(metricName, vme, nil)
			if err == nil && mvp.MetricAttrs.StatementTimeoutSeconds > 0 {
				stmtTimeoutOverride = mvp.MetricAttrs.StatementTimeoutSeconds
			} else {
				stmtTimeoutOverride = 0
			}
		}

		metricCurrentlyDisabled := IsMetricCurrentlyDisabledForHost(metricName, vme, dbUniqueName)
		if metricCurrentlyDisabled && opts.TestdataDays == 0 {
			logger.Debugf("[%s][%s] Ignoring fetch as metric disabled for current time range", dbUniqueName, metricName)
		} else {
			var metricStoreMessages []MetricStoreMessage
			var err error
			mfm := MetricFetchMessage{DBUniqueName: dbUniqueName, DBUniqueNameOrig: dbUniqueNameOrig, MetricName: metricName, DBType: dbType, Interval: time.Second * time.Duration(interval), StmtTimeoutOverride: stmtTimeoutOverride}

			// 1st try local overrides for some metrics if operating in push mode
			if opts.DirectOSStats && IsDirectlyFetchableMetric(metricName) {
				metricStoreMessages, err = FetchStatsDirectlyFromOS(mfm, vme, mvp)
				if err != nil {
					logger.Errorf("[%s][%s] Could not reader metric directly from OS: %v", dbUniqueName, metricName, err)
				}
			}
			t1 := time.Now()
			if metricStoreMessages == nil {
				metricStoreMessages, err = FetchMetrics(
					mfm,
					hostState,
					storeCh,
					"")
			}
			t2 := time.Now()

			if t2.Sub(t1) > (time.Second * time.Duration(interval)) {
				logger.Warningf("Total fetching time of %vs bigger than %vs interval for [%s:%s]", t2.Sub(t1).Truncate(time.Millisecond*100).Seconds(), interval, dbUniqueName, metricName)
			}

			if err != nil {
				failedFetches++
				// complain only 1x per 10min per host/metric...
				if lastErrorNotificationTime.IsZero() || lastErrorNotificationTime.Add(time.Second*time.Duration(600)).Before(time.Now()) {
					logger.Errorf("Failed to fetch metric data for [%s:%s]: %v", dbUniqueName, metricName, err)
					if failedFetches > 1 {
						logger.Errorf("Total failed fetches for [%s:%s]: %d", dbUniqueName, metricName, failedFetches)
					}
					lastErrorNotificationTime = time.Now()
				}
			} else if metricStoreMessages != nil {
				if opts.Metric.Datastore == datastorePrometheus && opts.Metric.PrometheusAsyncMode && len(metricStoreMessages[0].Data) == 0 {
					PurgeMetricsFromPromAsyncCacheIfAny(dbUniqueName, metricName)
				}
				if len(metricStoreMessages[0].Data) > 0 {

					// pick up "server restarted" events here to avoid doing extra selects from CheckForPGObjectChangesAndStore code
					if metricName == "db_stats" {
						postmasterUptimeS, ok := (metricStoreMessages[0].Data)[0]["postmaster_uptime_s"]
						if ok {
							if lastUptimeS != -1 {
								if postmasterUptimeS.(int64) < lastUptimeS { // restart (or possibly also failover when host is routed) happened
									message := "Detected server restart (or failover) of \"" + dbUniqueName + "\""
									logger.Warning(message)
									detectedChangesSummary := make(MetricData, 0)
									entry := MetricEntry{"details": message, "epoch_ns": (metricStoreMessages[0].Data)[0]["epoch_ns"]}
									detectedChangesSummary = append(detectedChangesSummary, entry)
									metricStoreMessages = append(metricStoreMessages,
										MetricStoreMessage{DBUniqueName: dbUniqueName, DBType: dbType,
											MetricName: "object_changes", Data: detectedChangesSummary, CustomTags: metricStoreMessages[0].CustomTags})
								}
							}
							lastUptimeS = postmasterUptimeS.(int64)
						}
					}

					if opts.TestdataDays != 0 {
						origMsgs := deepCopyMetricStoreMessages(metricStoreMessages)
						logger.Warningf("Generating %d days of data for [%s:%s]", opts.TestdataDays, dbUniqueName, metricName)
						testMetricsStored := 0
						simulatedTime := t1
						endTime := t1.Add(time.Hour * time.Duration(opts.TestdataDays*24))

						if opts.TestdataDays < 0 {
							simulatedTime, endTime = endTime, simulatedTime
						}

						for simulatedTime.Before(endTime) {
							logger.Debugf("Metric [%s], simulating time: %v", metricName, simulatedTime)
							for hostNr := 1; hostNr <= opts.TestdataMultiplier; hostNr++ {
								fakeDbName := fmt.Sprintf("%s-%d", dbUniqueName, hostNr)
								msgsCopyTmp := deepCopyMetricStoreMessages(origMsgs)

								for i := 0; i < len(msgsCopyTmp[0].Data); i++ {
									(msgsCopyTmp[0].Data)[i][epochColumnName] = (simulatedTime.UnixNano() + int64(1000*i))
								}
								msgsCopyTmp[0].DBUniqueName = fakeDbName
								//log.Debugf("fake data for [%s:%s]: %v", metricName, fake_dbname, msgs_copy_tmp[0].Data)
								_, _ = StoreMetrics(msgsCopyTmp, storeCh)
								testMetricsStored += len(msgsCopyTmp[0].Data)
							}
							time.Sleep(time.Duration(opts.TestdataMultiplier * 10000000)) // 10ms * multiplier (in nanosec).
							// would generate more metrics than persister can write and eat up RAM
							simulatedTime = simulatedTime.Add(time.Second * time.Duration(interval))
						}
						logger.Warningf("exiting MetricGathererLoop for [%s], %d total data points generated for %d hosts",
							metricName, testMetricsStored, opts.TestdataMultiplier)
						testDataGenerationModeWG.Done()
						return
					}
					_, _ = StoreMetrics(metricStoreMessages, storeCh)
				}
			}

			if opts.TestdataDays != 0 { // covers errors & no data
				testDataGenerationModeWG.Done()
				return
			}

		}
		select {
		case msg := <-controlCh:
			logger.Debug("got control msg", dbUniqueName, metricName, msg)
			if msg.Action == gathererStatusStart {
				config = msg.Config
				interval = config[metricName]
				if ticker != nil {
					ticker.Stop()
				}
				ticker = time.NewTicker(time.Millisecond * time.Duration(interval*1000))
				logger.Debug("started MetricGathererLoop for ", dbUniqueName, metricName, " interval:", interval)
			} else if msg.Action == gathererStatusStop {
				logger.Debug("exiting MetricGathererLoop for ", dbUniqueName, metricName, " interval:", interval)
				return
			}
		case <-ticker.C:
			logger.Debugf("MetricGathererLoop for [%s:%s] slept for %s", dbUniqueName, metricName, time.Second*time.Duration(interval))
		}

	}
}

func FetchStatsDirectlyFromOS(msg MetricFetchMessage, vme DBVersionMapEntry, mvp MetricVersionProperties) ([]MetricStoreMessage, error) {
	var data []map[string]any
	var err error

	if msg.MetricName == metricCPULoad { // could function pointers work here?
		data, err = psutil.GetLoadAvgLocal()
	} else if msg.MetricName == metricPsutilCPU {
		data, err = psutil.GetGoPsutilCPU(msg.Interval)
	} else if msg.MetricName == metricPsutilDisk {
		data, err = GetGoPsutilDiskPG(msg.DBUniqueName)
	} else if msg.MetricName == metricPsutilDiskIoTotal {
		data, err = psutil.GetGoPsutilDiskTotals()
	} else if msg.MetricName == metricPsutilMem {
		data, err = psutil.GetGoPsutilMem()
	}
	if err != nil {
		return nil, err
	}

	msm := DatarowsToMetricstoreMessage(data, msg, vme, mvp)
	return []MetricStoreMessage{msm}, nil
}

// data + custom tags + counters
func DatarowsToMetricstoreMessage(data MetricData, msg MetricFetchMessage, vme DBVersionMapEntry, mvp MetricVersionProperties) MetricStoreMessage {
	md, err := GetMonitoredDatabaseByUniqueName(msg.DBUniqueName)
	if err != nil {
		logger.Errorf("Could not resolve DBUniqueName %s, cannot set custom attributes for gathered data: %v", msg.DBUniqueName, err)
	}

	atomic.AddUint64(&totalMetricsFetchedCounter, uint64(len(data)))

	return MetricStoreMessage{
		DBUniqueName:            msg.DBUniqueName,
		DBType:                  msg.DBType,
		MetricName:              msg.MetricName,
		CustomTags:              md.CustomTags,
		Data:                    data,
		MetricDefinitionDetails: mvp,
		RealDbname:              vme.RealDbname,
		SystemIdentifier:        vme.SystemIdentifier,
	}
}

func IsDirectlyFetchableMetric(metric string) bool {
	if _, ok := directlyFetchableOSMetrics[metric]; ok {
		return true
	}
	return false
}

func IsStringInSlice(target string, slice []string) bool {
	for _, s := range slice {
		if target == s {
			return true
		}
	}
	return false
}

func IsMetricCurrentlyDisabledForHost(metricName string, vme DBVersionMapEntry, dbUniqueName string) bool {
	_, isSpecialMetric := specialMetrics[metricName]

	mvp, err := GetMetricVersionProperties(metricName, vme, nil)
	if err != nil {
		if isSpecialMetric || strings.Contains(err.Error(), "too old") {
			return false
		}
		logger.Warningf("[%s][%s] Ignoring any possible time based gathering restrictions, could not get metric details", dbUniqueName, metricName)
		return false
	}

	md, err := GetMonitoredDatabaseByUniqueName(dbUniqueName) // TODO caching?
	if err != nil {
		logger.Warningf("[%s][%s] Ignoring any possible time based gathering restrictions, could not get DB details", dbUniqueName, metricName)
		return false
	}

	if md.HostConfig.PerMetricDisabledTimes == nil && mvp.MetricAttrs.DisabledDays == "" && len(mvp.MetricAttrs.DisableTimes) == 0 {
		//log.Debugf("[%s][%s] No time based gathering restrictions defined", dbUniqueName, metricName)
		return false
	}

	metricHasOverrides := false
	if md.HostConfig.PerMetricDisabledTimes != nil {
		for _, hcdt := range md.HostConfig.PerMetricDisabledTimes {
			if IsStringInSlice(metricName, hcdt.Metrics) && (hcdt.DisabledDays != "" || len(hcdt.DisabledTimes) > 0) {
				metricHasOverrides = true
				break
			}
		}
		if !metricHasOverrides && mvp.MetricAttrs.DisabledDays == "" && len(mvp.MetricAttrs.DisableTimes) == 0 {
			//log.Debugf("[%s][%s] No time based gathering restrictions defined", dbUniqueName, metricName)
			return false
		}
	}

	return IsInDisabledTimeDayRange(time.Now(), mvp.MetricAttrs.DisabledDays, mvp.MetricAttrs.DisableTimes, md.HostConfig.PerMetricDisabledTimes, metricName, dbUniqueName)
}

// days: 0 = Sun, ranges allowed
func IsInDaySpan(locTime time.Time, days, _, _ string) bool {
	//log.Debug("IsInDaySpan", locTime, days, metric, dbUnique)
	if days == "" {
		return false
	}
	curDayInt := int(locTime.Weekday())
	daysMap := DaysStringToIntMap(days)
	//log.Debugf("curDayInt %v, daysMap %+v", curDayInt, daysMap)
	_, ok := daysMap[curDayInt]
	return ok
}

func DaysStringToIntMap(days string) map[int]bool { // TODO validate with some regex when reading in configs, have dbname info then
	ret := make(map[int]bool)
	for _, s := range strings.Split(days, ",") {
		if strings.Contains(s, "-") {
			dayRange := strings.Split(s, "-")
			if len(dayRange) != 2 {
				logger.Warningf("Ignoring invalid day range specification: %s. Check config", s)
				continue
			}
			startDay, err := strconv.Atoi(dayRange[0])
			endDay, err2 := strconv.Atoi(dayRange[1])
			if err != nil || err2 != nil {
				logger.Warningf("Ignoring invalid day range specification: %s. Check config", s)
				continue
			}
			for i := startDay; i <= endDay && i >= 0 && i <= 7; i++ {
				ret[i] = true
			}

		} else {
			day, err := strconv.Atoi(s)
			if err != nil {
				logger.Warningf("Ignoring invalid day range specification: %s. Check config", days)
				continue
			}
			ret[day] = true
		}
	}
	if _, ok := ret[7]; ok { // Cron allows either 0 or 7 for Sunday
		ret[0] = true
	}
	return ret
}

func IsInTimeSpan(checkTime time.Time, timeRange, metric, dbUnique string) bool {
	layout := "15:04"
	var t1, t2 time.Time
	var err error

	timeRange = strings.TrimSpace(timeRange)
	if len(timeRange) < 11 {
		logger.Warningf("[%s][%s] invalid time range: %s. Check config", dbUnique, metric, timeRange)
		return false
	}
	s1 := timeRange[0:5]
	s2 := timeRange[6:11]
	tz := strings.TrimSpace(timeRange[11:])

	if len(tz) > 1 { // time zone specified
		if regexIsAlpha.MatchString(tz) {
			layout = "15:04 MST"
		} else {
			layout = "15:04 -0700"
		}
		t1, err = time.Parse(layout, s1+" "+tz)
		if err == nil {
			t2, err = time.Parse(layout, s2+" "+tz)
		}
	} else { // no time zone
		t1, err = time.Parse(layout, s1)
		if err == nil {
			t2, err = time.Parse(layout, s2)
		}
	}

	if err != nil {
		logger.Warningf("[%s][%s] Ignoring invalid disabled time range: %s. Check config. Erorr: %v", dbUnique, metric, timeRange, err)
		return false
	}

	check, err := time.Parse("15:04 -0700", strconv.Itoa(checkTime.Hour())+":"+strconv.Itoa(checkTime.Minute())+" "+t1.Format("-0700")) // UTC by default
	if err != nil {
		logger.Warningf("[%s][%s] Ignoring invalid disabled time range: %s. Check config. Error: %v", dbUnique, metric, timeRange, err)
		return false
	}

	if t1.After(t2) {
		t2 = t2.AddDate(0, 0, 1)
	}

	return check.Before(t2) && check.After(t1)
}

func IsInDisabledTimeDayRange(localTime time.Time, metricAttrsDisabledDays string, metricAttrsDisabledTimes []string, hostConfigPerMetricDisabledTimes []HostConfigPerMetricDisabledTimes, metric, dbUnique string) bool {
	hostConfigMetricMatch := false
	for _, hcdi := range hostConfigPerMetricDisabledTimes { // host config takes precedence when both specified
		dayMatchFound := false
		timeMatchFound := false
		if IsStringInSlice(metric, hcdi.Metrics) {
			hostConfigMetricMatch = true
			if !dayMatchFound && hcdi.DisabledDays != "" && IsInDaySpan(localTime, hcdi.DisabledDays, metric, dbUnique) {
				dayMatchFound = true
			}
			for _, dt := range hcdi.DisabledTimes {
				if IsInTimeSpan(localTime, dt, metric, dbUnique) {
					timeMatchFound = true
					break
				}
			}
		}
		if hostConfigMetricMatch && (timeMatchFound || len(hcdi.DisabledTimes) == 0) && (dayMatchFound || hcdi.DisabledDays == "") {
			//log.Debugf("[%s][%s] Host config ignored time/day match, skipping fetch", dbUnique, metric)
			return true
		}
	}

	if !hostConfigMetricMatch && (metricAttrsDisabledDays != "" || len(metricAttrsDisabledTimes) > 0) {
		dayMatchFound := IsInDaySpan(localTime, metricAttrsDisabledDays, metric, dbUnique)
		timeMatchFound := false
		for _, timeRange := range metricAttrsDisabledTimes {
			if IsInTimeSpan(localTime, timeRange, metric, dbUnique) {
				timeMatchFound = true
				break
			}
		}
		if (timeMatchFound || len(metricAttrsDisabledTimes) == 0) && (dayMatchFound || metricAttrsDisabledDays == "") {
			//log.Debugf("[%s][%s] MetricAttrs ignored time/day match, skipping fetch", dbUnique, metric)
			return true
		}
	}

	return false
}

func UpdateMetricDefinitionMap(newMetrics map[string]map[decimal.Decimal]MetricVersionProperties) {
	metricDefMapLock.Lock()
	metricDefinitionMap = newMetrics
	metricDefMapLock.Unlock()
	//log.Debug("metric_def_map:", metric_def_map)
	logger.Debug("metrics definitions refreshed - nr. found:", len(newMetrics))
}

func jsonTextToMap(jsonText string) (map[string]float64, error) {
	retmap := make(map[string]float64)
	if jsonText == "" {
		return retmap, nil
	}
	var hostConfig map[string]any
	if err := json.Unmarshal([]byte(jsonText), &hostConfig); err != nil {
		return nil, err
	}
	for k, v := range hostConfig {
		retmap[k] = v.(float64)
	}
	return retmap, nil
}

func jsonTextToStringMap(jsonText string) (map[string]string, error) {
	retmap := make(map[string]string)
	if jsonText == "" {
		return retmap, nil
	}
	var iMap map[string]any
	if err := json.Unmarshal([]byte(jsonText), &iMap); err != nil {
		return nil, err
	}
	for k, v := range iMap {
		retmap[k] = fmt.Sprintf("%v", v)
	}
	return retmap, nil
}

// Expects "preset metrics" definition file named preset-config.yaml to be present in provided --metrics folder
func ReadPresetMetricsConfigFromFolder(folder string, _ bool) (map[string]map[string]float64, error) {
	pmm := make(map[string]map[string]float64)

	logger.Infof("Reading preset metric config from path %s ...", path.Join(folder, presetConfigYAMLFile))
	presetMetrics, err := os.ReadFile(path.Join(folder, presetConfigYAMLFile))
	if err != nil {
		logger.Errorf("Failed to read preset metric config definition at: %s", folder)
		return pmm, err
	}
	pcs := make([]PresetConfig, 0)
	err = yaml.Unmarshal(presetMetrics, &pcs)
	if err != nil {
		logger.Errorf("Unmarshaling error reading preset metric config: %v", err)
		return pmm, err
	}
	for _, pc := range pcs {
		pmm[pc.Name] = pc.Metrics
	}
	logger.Infof("%d preset metric definitions found", len(pcs))
	return pmm, err
}

func ParseMetricColumnAttrsFromYAML(yamlPath string) MetricColumnAttrs {
	c := MetricColumnAttrs{}

	yamlFile, err := os.ReadFile(yamlPath)
	if err != nil {
		logger.Errorf("Error reading file %s: %s", yamlFile, err)
		return c
	}

	err = yaml.Unmarshal(yamlFile, &c)
	if err != nil {
		logger.Errorf("Unmarshaling error: %v", err)
	}
	return c
}

func ParseMetricAttrsFromYAML(yamlPath string) MetricAttrs {
	c := MetricAttrs{}

	yamlFile, err := os.ReadFile(yamlPath)
	if err != nil {
		logger.Errorf("Error reading file %s: %s", yamlFile, err)
		return c
	}

	err = yaml.Unmarshal(yamlFile, &c)
	if err != nil {
		logger.Errorf("Unmarshaling error: %v", err)
	}
	return c
}

func ParseMetricColumnAttrsFromString(jsonAttrs string) MetricColumnAttrs {
	c := MetricColumnAttrs{}

	err := yaml.Unmarshal([]byte(jsonAttrs), &c)
	if err != nil {
		logger.Errorf("Unmarshaling error: %v", err)
	}
	return c
}

func ParseMetricAttrsFromString(jsonAttrs string) MetricAttrs {
	c := MetricAttrs{}

	err := yaml.Unmarshal([]byte(jsonAttrs), &c)
	if err != nil {
		logger.Errorf("Unmarshaling error: %v", err)
	}
	return c
}

// expected is following structure: metric_name/pg_ver/metric(_master|standby).sql
func ReadMetricsFromFolder(folder string, failOnError bool) (map[string]map[decimal.Decimal]MetricVersionProperties, error) {
	metricsMap := make(map[string]map[decimal.Decimal]MetricVersionProperties)
	metricNameRemapsNew := make(map[string]string)
	rIsDigitOrPunctuation := regexp.MustCompile(`^[\d\.]+$`)
	metricNamePattern := `^[a-z0-9_\.]+$`
	rMetricNameFilter := regexp.MustCompile(metricNamePattern)

	logger.Infof("Searching for metrics from path %s ...", folder)
	metricFolders, err := os.ReadDir(folder)
	if err != nil {
		if failOnError {
			logger.Fatalf("Could not read path %s: %s", folder, err)
		}
		logger.Error(err)
		return metricsMap, err
	}

	for _, f := range metricFolders {
		if f.IsDir() {
			if f.Name() == fileBasedMetricHelpersDir {
				continue // helpers are pulled in when needed
			}
			if !rMetricNameFilter.MatchString(f.Name()) {
				logger.Warningf("Ignoring metric '%s' as name not fitting pattern: %s", f.Name(), metricNamePattern)
				continue
			}
			//log.Debugf("Processing metric: %s", f.Name())
			pgVers, err := os.ReadDir(path.Join(folder, f.Name()))
			if err != nil {
				logger.Error(err)
				return metricsMap, err
			}

			var metricAttrs MetricAttrs
			if _, err = os.Stat(path.Join(folder, f.Name(), "metric_attrs.yaml")); err == nil {
				metricAttrs = ParseMetricAttrsFromYAML(path.Join(folder, f.Name(), "metric_attrs.yaml"))
				//log.Debugf("Discovered following metric attributes for metric %s: %v", f.Name(), metricAttrs)
				if metricAttrs.MetricStorageName != "" {
					metricNameRemapsNew[f.Name()] = metricAttrs.MetricStorageName
				}
			}

			var metricColumnAttrs MetricColumnAttrs
			if _, err = os.Stat(path.Join(folder, f.Name(), "column_attrs.yaml")); err == nil {
				metricColumnAttrs = ParseMetricColumnAttrsFromYAML(path.Join(folder, f.Name(), "column_attrs.yaml"))
				//log.Debugf("Discovered following column attributes for metric %s: %v", f.Name(), metricColumnAttrs)
			}

			for _, pgVer := range pgVers {
				if strings.HasSuffix(pgVer.Name(), ".md") || pgVer.Name() == "column_attrs.yaml" || pgVer.Name() == "metric_attrs.yaml" {
					continue
				}
				if !rIsDigitOrPunctuation.MatchString(pgVer.Name()) {
					logger.Warningf("Invalid metric structure - version folder names should consist of only numerics/dots, found: %s", pgVer.Name())
					continue
				}
				dirName, err := decimal.NewFromString(pgVer.Name())
				if err != nil {
					logger.Errorf("Could not parse \"%s\" to Decimal: %s", pgVer.Name(), err)
					continue
				}
				//log.Debugf("Found %s", pgVer.Name())

				metricDefs, err := os.ReadDir(path.Join(folder, f.Name(), pgVer.Name()))
				if err != nil {
					logger.Error(err)
					continue
				}

				foundMetricDefFiles := make(map[string]bool) // to warn on accidental duplicates
				for _, md := range metricDefs {
					if strings.HasPrefix(md.Name(), "metric") && strings.HasSuffix(md.Name(), ".sql") {
						p := path.Join(folder, f.Name(), pgVer.Name(), md.Name())
						metricSQL, err := os.ReadFile(p)
						if err != nil {
							logger.Errorf("Failed to read metric definition at: %s", p)
							continue
						}
						_, exists := foundMetricDefFiles[md.Name()]
						if exists {
							logger.Warningf("Multiple definitions found for metric [%s:%s], using the last one (%s)...", f.Name(), pgVer.Name(), md.Name())
						}
						foundMetricDefFiles[md.Name()] = true

						//log.Debugf("Metric definition for \"%s\" ver %s: %s", f.Name(), pgVer.Name(), metric_sql)
						mvpVer, ok := metricsMap[f.Name()]
						var mvp MetricVersionProperties
						if !ok {
							metricsMap[f.Name()] = make(map[decimal.Decimal]MetricVersionProperties)
						}
						mvp, ok = mvpVer[dirName]
						if !ok {
							mvp = MetricVersionProperties{SQL: string(metricSQL[:]), ColumnAttrs: metricColumnAttrs, MetricAttrs: metricAttrs}
						}
						mvp.CallsHelperFunctions = DoesMetricDefinitionCallHelperFunctions(mvp.SQL)
						if strings.Contains(md.Name(), "_master") {
							mvp.MasterOnly = true
						}
						if strings.Contains(md.Name(), "_standby") {
							mvp.StandbyOnly = true
						}
						if strings.Contains(md.Name(), "_su") {
							mvp.SQLSU = string(metricSQL[:])
						}
						metricsMap[f.Name()][dirName] = mvp
					}
				}
			}
		}
	}

	metricNameRemapLock.Lock()
	metricNameRemaps = metricNameRemapsNew
	metricNameRemapLock.Unlock()

	return metricsMap, nil
}

func ExpandEnvVarsForConfigEntryIfStartsWithDollar(md MonitoredDatabase) (MonitoredDatabase, int) {
	var changed int

	if strings.HasPrefix(md.DBName, "$") {
		md.DBName = os.ExpandEnv(md.DBName)
		changed++
	}
	if strings.HasPrefix(md.User, "$") {
		md.User = os.ExpandEnv(md.User)
		changed++
	}
	if strings.HasPrefix(md.Password, "$") {
		md.Password = os.ExpandEnv(md.Password)
		changed++
	}
	if strings.HasPrefix(md.PasswordType, "$") {
		md.PasswordType = os.ExpandEnv(md.PasswordType)
		changed++
	}
	if strings.HasPrefix(md.DBType, "$") {
		md.DBType = os.ExpandEnv(md.DBType)
		changed++
	}
	if strings.HasPrefix(md.DBUniqueName, "$") {
		md.DBUniqueName = os.ExpandEnv(md.DBUniqueName)
		changed++
	}
	if strings.HasPrefix(md.SslMode, "$") {
		md.SslMode = os.ExpandEnv(md.SslMode)
		changed++
	}
	if strings.HasPrefix(md.DBNameIncludePattern, "$") {
		md.DBNameIncludePattern = os.ExpandEnv(md.DBNameIncludePattern)
		changed++
	}
	if strings.HasPrefix(md.DBNameExcludePattern, "$") {
		md.DBNameExcludePattern = os.ExpandEnv(md.DBNameExcludePattern)
		changed++
	}
	if strings.HasPrefix(md.PresetMetrics, "$") {
		md.PresetMetrics = os.ExpandEnv(md.PresetMetrics)
		changed++
	}
	if strings.HasPrefix(md.PresetMetricsStandby, "$") {
		md.PresetMetricsStandby = os.ExpandEnv(md.PresetMetricsStandby)
		changed++
	}

	return md, changed
}

func ConfigFileToMonitoredDatabases(configFilePath string) ([]MonitoredDatabase, error) {
	hostList := make([]MonitoredDatabase, 0)

	logger.Debugf("Converting monitoring YAML config to MonitoredDatabase from path %s ...", configFilePath)
	yamlFile, err := os.ReadFile(configFilePath)
	if err != nil {
		logger.Errorf("Error reading file %s: %s", configFilePath, err)
		return hostList, err
	}
	// TODO check mod timestamp or hash, from a global "caching map"
	c := make([]MonitoredDatabase, 0) // there can be multiple configs in a single file
	yamlFile = []byte(string(yamlFile))
	err = yaml.Unmarshal(yamlFile, &c)
	if err != nil {
		logger.Errorf("Unmarshaling error: %v", err)
		return hostList, err
	}
	for _, v := range c {
		if v.Port == "" {
			v.Port = "5432"
		}
		if v.DBType == "" {
			v.DBType = config.DbTypePg
		}
		if v.IsEnabled {
			logger.Debugf("Found active monitoring config entry: %#v", v)
			if v.Group == "" {
				v.Group = "default"
			}
			if v.StmtTimeout == 0 {
				v.StmtTimeout = 5
			}
			vExp, changed := ExpandEnvVarsForConfigEntryIfStartsWithDollar(v)
			if changed > 0 {
				logger.Debugf("[%s] %d config attributes expanded from ENV", vExp.DBUniqueName, changed)
			}
			hostList = append(hostList, vExp)
		}
	}
	if len(hostList) == 0 {
		logger.Warningf("Could not find any valid monitoring configs from file: %s", configFilePath)
	}
	return hostList, nil
}

// reads through the YAML files containing descriptions on which hosts to monitor
func ReadMonitoringConfigFromFileOrFolder(fileOrFolder string) ([]MonitoredDatabase, error) {
	hostList := make([]MonitoredDatabase, 0)

	fi, err := os.Stat(fileOrFolder)
	if err != nil {
		logger.Errorf("Could not Stat() path: %s", fileOrFolder)
		return hostList, err
	}
	switch mode := fi.Mode(); {
	case mode.IsDir():
		logger.Infof("Reading monitoring config from path %s ...", fileOrFolder)

		err := filepath.Walk(fileOrFolder, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err // abort on first failure
			}
			if info.Mode().IsRegular() && (strings.HasSuffix(strings.ToLower(info.Name()), ".yaml") || strings.HasSuffix(strings.ToLower(info.Name()), ".yml")) {
				logger.Debug("Found YAML config file:", info.Name())
				mdbs, err := ConfigFileToMonitoredDatabases(path)
				if err == nil {
					hostList = append(hostList, mdbs...)
				}
			}
			return nil
		})
		if err != nil {
			logger.Errorf("Could not successfully Walk() path %s: %s", fileOrFolder, err)
			return hostList, err
		}
	case mode.IsRegular():
		hostList, err = ConfigFileToMonitoredDatabases(fileOrFolder)
	}

	return hostList, err
}

// Resolves regexes if exact DBs were not specified exact
func GetMonitoredDatabasesFromMonitoringConfig(mc []MonitoredDatabase) []MonitoredDatabase {
	md := make([]MonitoredDatabase, 0)
	if len(mc) == 0 {
		return md
	}
	for _, e := range mc {
		//log.Debugf("Processing config item: %#v", e)
		if e.Metrics == nil && len(e.PresetMetrics) > 0 {
			mdef, ok := presetMetricDefMap[e.PresetMetrics]
			if !ok {
				logger.Errorf("Failed to resolve preset config \"%s\" for \"%s\"", e.PresetMetrics, e.DBUniqueName)
				continue
			}
			e.Metrics = mdef
		}
		if _, ok := dbTypeMap[e.DBType]; !ok {
			logger.Warningf("Ignoring host \"%s\" - unknown dbtype: %s. Expected one of: %+v", e.DBUniqueName, e.DBType, dbTypes)
			continue
		}
		if e.IsEnabled && e.PasswordType == "aes-gcm-256" && opts.AesGcmKeyphrase != "" {
			e.Password = decrypt(e.DBUniqueName, opts.AesGcmKeyphrase, e.Password)
		}
		if e.DBType == config.DbTypePatroni && e.DBName == "" {
			logger.Warningf("Ignoring host \"%s\" as \"dbname\" attribute not specified but required by dbtype=patroni", e.DBUniqueName)
			continue
		}
		if e.DBType == config.DbTypePg && e.DBName == "" {
			logger.Warningf("Ignoring host \"%s\" as \"dbname\" attribute not specified but required by dbtype=postgres", e.DBUniqueName)
			continue
		}
		if len(e.DBName) == 0 || e.DBType == config.DbTypePgCont || e.DBType == config.DbTypePatroni || e.DBType == config.DbTypePatroniCont || e.DBType == config.DbTypePatroniNamespaceDiscovery {
			if e.DBType == config.DbTypePgCont {
				logger.Debugf("Adding \"%s\" (host=%s, port=%s) to continuous monitoring ...", e.DBUniqueName, e.Host, e.Port)
			}
			var foundDbs []MonitoredDatabase
			var err error

			if e.DBType == config.DbTypePatroni || e.DBType == config.DbTypePatroniCont || e.DBType == config.DbTypePatroniNamespaceDiscovery {
				foundDbs, err = ResolveDatabasesFromPatroni(e)
			} else {
				foundDbs, err = ResolveDatabasesFromConfigEntry(e)
			}
			if err != nil {
				logger.Errorf("Failed to resolve DBs for \"%s\": %s", e.DBUniqueName, err)
				continue
			}
			tempArr := make([]string, 0)
			for _, r := range foundDbs {
				md = append(md, r)
				tempArr = append(tempArr, r.DBName)
			}
			logger.Debugf("Resolved %d DBs with prefix \"%s\": [%s]", len(foundDbs), e.DBUniqueName, strings.Join(tempArr, ", "))
		} else {
			md = append(md, e)
		}
	}
	return md
}

func StatsServerHandler(w http.ResponseWriter, _ *http.Request) {
	jsonResponseTemplate := `
{
	"secondsFromLastSuccessfulDatastoreWrite": %d,
	"totalMetricsFetchedCounter": %d,
	"totalMetricsReusedFromCacheCounter": %d,
	"totalDatasetsFetchedCounter": %d,
	"metricPointsPerMinuteLast5MinAvg": %v,
	"metricsDropped": %d,
	"totalMetricFetchFailuresCounter": %d,
	"datastoreWriteFailuresCounter": %d,
	"datastoreSuccessfulWritesCounter": %d,
	"datastoreAvgSuccessfulWriteTimeMillis": %.1f,
	"databasesMonitored": %d,
	"databasesConfigured": %d,
	"unreachableDBs": %d,
	"gathererUptimeSeconds": %d
}
`
	now := time.Now()
	secondsFromLastSuccessfulDatastoreWrite := atomic.LoadInt64(&lastSuccessfulDatastoreWriteTimeEpoch)
	totalMetrics := atomic.LoadUint64(&totalMetricsFetchedCounter)
	cacheMetrics := atomic.LoadUint64(&totalMetricsReusedFromCacheCounter)
	totalDatasets := atomic.LoadUint64(&totalDatasetsFetchedCounter)
	metricsDropped := atomic.LoadUint64(&totalMetricsDroppedCounter)
	metricFetchFailuresCounter := atomic.LoadUint64(&totalMetricFetchFailuresCounter)
	datastoreFailures := atomic.LoadUint64(&datastoreWriteFailuresCounter)
	datastoreSuccess := atomic.LoadUint64(&datastoreWriteSuccessCounter)
	datastoreTotalTimeMicros := atomic.LoadUint64(&datastoreTotalWriteTimeMicroseconds) // successful writes only
	datastoreAvgSuccessfulWriteTimeMillis := float64(datastoreTotalTimeMicros) / float64(datastoreSuccess) / 1000.0
	gathererUptimeSeconds := uint64(now.Sub(gathererStartTime).Seconds())
	var metricPointsPerMinute int64
	metricPointsPerMinute = atomic.LoadInt64(&metricPointsPerMinuteLast5MinAvg)
	if metricPointsPerMinute == -1 { // calculate avg. on the fly if 1st summarization hasn't happened yet
		metricPointsPerMinute = int64((totalMetrics * 60) / gathererUptimeSeconds)
	}
	monitoredDbs := getMonitoredDatabasesSnapshot()
	databasesConfigured := len(monitoredDbs) // including replicas
	databasesMonitored := 0
	for _, md := range monitoredDbs {
		if shouldDbBeMonitoredBasedOnCurrentState(md) {
			databasesMonitored++
		}
	}
	unreachableDBsLock.RLock()
	unreachableDBs := len(unreachableDB)
	unreachableDBsLock.RUnlock()
	_, _ = io.WriteString(w, fmt.Sprintf(jsonResponseTemplate, time.Now().Unix()-secondsFromLastSuccessfulDatastoreWrite, totalMetrics, cacheMetrics, totalDatasets, metricPointsPerMinute, metricsDropped, metricFetchFailuresCounter, datastoreFailures, datastoreSuccess, datastoreAvgSuccessfulWriteTimeMillis, databasesMonitored, databasesConfigured, unreachableDBs, gathererUptimeSeconds))
}

func StartStatsServer(port int64) {
	http.HandleFunc("/", StatsServerHandler)
	for {
		logger.Errorf("Failure in StatsServerHandler:", http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
		time.Sleep(time.Second * 60)
	}
}

// Calculates 1min avg metric fetching statistics for last 5min for StatsServerHandler to display
func StatsSummarizer() {
	var prevMetricsCounterValue uint64
	var currentMetricsCounterValue uint64
	ticker := time.NewTicker(time.Minute * 5)
	lastSummarization := gathererStartTime
	for now := range ticker.C {
		currentMetricsCounterValue = atomic.LoadUint64(&totalMetricsFetchedCounter)
		atomic.StoreInt64(&metricPointsPerMinuteLast5MinAvg, int64(math.Round(float64(currentMetricsCounterValue-prevMetricsCounterValue)*60/now.Sub(lastSummarization).Seconds())))
		prevMetricsCounterValue = currentMetricsCounterValue
		lastSummarization = now
	}
}

func FilterMonitoredDatabasesByGroup(monitoredDBs []MonitoredDatabase, group string) ([]MonitoredDatabase, int) {
	ret := make([]MonitoredDatabase, 0)
	groups := strings.Split(group, ",")
	for _, md := range monitoredDBs {
		// matched := false
		for _, g := range groups {
			if md.Group == g {
				ret = append(ret, md)
				break
			}
		}
	}
	return ret, len(monitoredDBs) - len(ret)
}

func encrypt(passphrase, plaintext string) string { // called when --password-to-encrypt set
	key, salt := deriveKey(passphrase, nil)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	b, _ := aes.NewCipher(key)
	aesgcm, _ := cipher.NewGCM(b)
	data := aesgcm.Seal(nil, iv, []byte(plaintext), nil)
	return hex.EncodeToString(salt) + "-" + hex.EncodeToString(iv) + "-" + hex.EncodeToString(data)
}

func deriveKey(passphrase string, salt []byte) ([]byte, []byte) {
	if salt == nil {
		salt = make([]byte, 8)
		_, _ = rand.Read(salt)
	}
	return pbkdf2.Key([]byte(passphrase), salt, 1000, 32, sha256.New), salt
}

func decrypt(dbUnique, passphrase, ciphertext string) string {
	arr := strings.Split(ciphertext, "-")
	if len(arr) != 3 {
		logger.Warningf("Aes-gcm-256 encrypted password for \"%s\" should consist of 3 parts - using 'as is'", dbUnique)
		return ciphertext
	}
	salt, _ := hex.DecodeString(arr[0])
	iv, _ := hex.DecodeString(arr[1])
	data, _ := hex.DecodeString(arr[2])
	key, _ := deriveKey(passphrase, salt)
	b, _ := aes.NewCipher(key)
	aesgcm, _ := cipher.NewGCM(b)
	data, _ = aesgcm.Open(nil, iv, data, nil)
	//log.Debug("decoded", string(data))
	return string(data)
}

func SyncMonitoredDBsToDatastore(monitoredDbs []MonitoredDatabase, persistenceChannel chan []MetricStoreMessage) {
	if len(monitoredDbs) > 0 {
		msms := make([]MetricStoreMessage, len(monitoredDbs))
		now := time.Now()

		for _, mdb := range monitoredDbs {
			var db = make(MetricEntry)
			db["tag_group"] = mdb.Group
			db["master_only"] = mdb.OnlyIfMaster
			db["epoch_ns"] = now.UnixNano()
			db["continuous_discovery_prefix"] = mdb.DBUniqueNameOrig
			for k, v := range mdb.CustomTags {
				db["tag_"+k] = v
			}
			var data = MetricData{db}
			msms = append(msms, MetricStoreMessage{DBUniqueName: mdb.DBUniqueName, MetricName: monitoredDbsDatastoreSyncMetricName,
				Data: data})
		}
		persistenceChannel <- msms
	}
}

func CheckFolderExistsAndReadable(path string) bool {
	if _, err := os.ReadDir(path); err != nil {
		return false
	}
	return true
}

func shouldDbBeMonitoredBasedOnCurrentState(md MonitoredDatabase) bool {
	return !IsDBDormant(md.DBUniqueName)
}

func ControlChannelsMapToList(controlChannels map[string]chan ControlMessage) []string {
	controlChannelList := make([]string, len(controlChannels))
	i := 0
	for key := range controlChannels {
		controlChannelList[i] = key
		i++
	}
	return controlChannelList
}

func DoCloseResourcesForRemovedMonitoredDBIfAny(dbUnique string) {

	CloseOrLimitSQLConnPoolForMonitoredDBIfAny(dbUnique)

	PurgeMetricsFromPromAsyncCacheIfAny(dbUnique, "")
}

func CloseResourcesForRemovedMonitoredDBs(currentDBs, prevLoopDBs []MonitoredDatabase, shutDownDueToRoleChange map[string]bool) {
	var curDBsMap = make(map[string]bool)

	for _, curDB := range currentDBs {
		curDBsMap[curDB.DBUniqueName] = true
	}

	for _, prevDB := range prevLoopDBs {
		if _, ok := curDBsMap[prevDB.DBUniqueName]; !ok { // removed from config
			DoCloseResourcesForRemovedMonitoredDBIfAny(prevDB.DBUniqueName)
		}
	}

	// or to be ignored due to current instance state
	for roleChangedDB := range shutDownDueToRoleChange {
		DoCloseResourcesForRemovedMonitoredDBIfAny(roleChangedDB)
	}
}

func PromAsyncCacheInitIfRequired(dbUnique, _ string) { // cache structure: [dbUnique][metric]lastly_fetched_data
	if opts.Metric.Datastore == datastorePrometheus && opts.Metric.PrometheusAsyncMode {
		promAsyncMetricCacheLock.Lock()
		defer promAsyncMetricCacheLock.Unlock()
		if _, ok := promAsyncMetricCache[dbUnique]; !ok {
			metricMap := make(map[string][]MetricStoreMessage)
			promAsyncMetricCache[dbUnique] = metricMap
		}
	}
}

func PromAsyncCacheAddMetricData(dbUnique, metric string, msgArr []MetricStoreMessage) { // cache structure: [dbUnique][metric]lastly_fetched_data
	promAsyncMetricCacheLock.Lock()
	defer promAsyncMetricCacheLock.Unlock()
	if _, ok := promAsyncMetricCache[dbUnique]; ok {
		promAsyncMetricCache[dbUnique][metric] = msgArr
	}
}

func SetUndersizedDBState(dbUnique string, state bool) {
	undersizedDBsLock.Lock()
	undersizedDBs[dbUnique] = state
	undersizedDBsLock.Unlock()
}

func IsDBUndersized(dbUnique string) bool {
	undersizedDBsLock.RLock()
	defer undersizedDBsLock.RUnlock()
	undersized, ok := undersizedDBs[dbUnique]
	if ok {
		return undersized
	}
	return false
}

func SetRecoveryIgnoredDBState(dbUnique string, state bool) {
	recoveryIgnoredDBsLock.Lock()
	recoveryIgnoredDBs[dbUnique] = state
	recoveryIgnoredDBsLock.Unlock()
}

func IsDBIgnoredBasedOnRecoveryState(dbUnique string) bool {
	recoveryIgnoredDBsLock.RLock()
	defer recoveryIgnoredDBsLock.RUnlock()
	recoveryIgnored, ok := undersizedDBs[dbUnique]
	if ok {
		return recoveryIgnored
	}
	return false
}

func IsDBDormant(dbUnique string) bool {
	return IsDBUndersized(dbUnique) || IsDBIgnoredBasedOnRecoveryState(dbUnique)
}

func DoesEmergencyTriggerfileExist() bool {
	// Main idea of the feature is to be able to quickly free monitored DBs / network of any extra "monitoring effect" load.
	// In highly automated K8s / IaC environments such a temporary change might involve pull requests, peer reviews, CI/CD etc
	// which can all take too long vs "exec -it pgwatch3-pod -- touch /tmp/pgwatch3-emergency-pause".
	// NB! After creating the file it can still take up to --servers-refresh-loop-seconds (2min def.) for change to take effect!
	if opts.EmergencyPauseTriggerfile == "" {
		return false
	}
	_, err := os.Stat(opts.EmergencyPauseTriggerfile)
	return err == nil
}

func DoesMetricDefinitionCallHelperFunctions(sqlDefinition string) bool {
	if !opts.Metric.NoHelperFunctions { // save on regex matching --no-helper-functions param not set, information will not be used then anyways
		return false
	}
	return regexSQLHelperFunctionCalled.MatchString(sqlDefinition)
}

var opts config.CmdOptions

// version output variables
var (
	commit  = "000000"
	version = "master"
	date    = "unknown"
	dbapi   = "00534"
)

func printVersion() {
	fmt.Printf(`pgwatch3:
  Version:      %s
  DB Schema:    %s
  Git Commit:   %s
  Built:        %s
`, version, dbapi, commit, date)
}

// SetupCloseHandler creates a 'listener' on a new goroutine which will notify the
// program if it receives an interrupt from the OS. We then handle this by calling
// our clean up procedure and exiting the program.
func SetupCloseHandler(cancel context.CancelFunc) {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		cancel()
	}()
	exitCode = ExitCodeUserCancel
}

const (
	ExitCodeOK int = iota
	ExitCodeConfigError
	ExitCodeWebUIError
	ExitCodeUpgradeError
	ExitCodeUserCancel
	ExitCodeShutdownCommand
)

var exitCode = ExitCodeOK

func main() {
	defer func() { os.Exit(exitCode) }()

	ctx, cancel := context.WithCancel(context.Background())
	SetupCloseHandler(cancel)
	defer cancel()

	opts, err := config.NewConfig(os.Stdout)
	if err != nil {
		if opts != nil && opts.VersionOnly() {
			printVersion()
			return
		}
		fmt.Println("Configuration error: ", err)
		exitCode = ExitCodeConfigError
		return
	}

	logger = log.Init(opts.Logging)

	uifs, _ := fs.Sub(webuifs, "webui/build")
	ui := webserver.Init(":8080", uifs, uiapi, logger)
	if ui == nil {
		exitCode = ExitCodeWebUIError
		return
	}

	logger.Debugf("opts: %+v", opts)

	if opts.AesGcmPasswordToEncrypt > "" { // special flag - encrypt and exit
		fmt.Println(encrypt(opts.AesGcmKeyphrase, opts.AesGcmPasswordToEncrypt))
		return
	}

	if opts.IsAdHocMode() && opts.AdHocUniqueName == "adhoc" {
		logger.Warning("In ad-hoc mode: using default unique name 'adhoc' for metrics storage. use --adhoc-name to override.")
	}

	// running in config file based mode?
	if len(opts.Config) > 0 {
		if opts.Metric.MetricsFolder == "" && CheckFolderExistsAndReadable(defaultMetricsDefinitionPathPkg) {
			opts.Metric.MetricsFolder = defaultMetricsDefinitionPathPkg
			logger.Warningf("--metrics-folder path not specified, using %s", opts.Metric.MetricsFolder)
		} else if opts.Metric.MetricsFolder == "" && CheckFolderExistsAndReadable(defaultMetricsDefinitionPathDocker) {
			opts.Metric.MetricsFolder = defaultMetricsDefinitionPathDocker
			logger.Warningf("--metrics-folder path not specified, using %s", opts.Metric.MetricsFolder)
		} else {
			if !CheckFolderExistsAndReadable(opts.Metric.MetricsFolder) {
				logger.Fatalf("Could not read --metrics-folder path %s", opts.Metric.MetricsFolder)
			}
		}

		if !opts.IsAdHocMode() {
			fi, err := os.Stat(opts.Config)
			if err != nil {
				logger.Fatalf("Could not Stat() path %s: %s", opts.Config, err)
			}
			switch mode := fi.Mode(); {
			case mode.IsDir():
				_, err := os.ReadDir(opts.Config)
				if err != nil {
					logger.Fatalf("Could not read path %s: %s", opts.Config, err)
				}
			case mode.IsRegular():
				_, err := os.ReadFile(opts.Config)
				if err != nil {
					logger.Fatalf("Could not read path %s: %s", opts.Config, err)
				}
			}
		}

		fileBasedMetrics = true
	} else if opts.IsAdHocMode() && opts.Metric.MetricsFolder != "" && CheckFolderExistsAndReadable(opts.Metric.MetricsFolder) {
		// don't need the Config DB connection actually for ad-hoc mode if metric definitions are there
		fileBasedMetrics = true
	} else if opts.IsAdHocMode() && opts.Metric.MetricsFolder == "" && (CheckFolderExistsAndReadable(defaultMetricsDefinitionPathPkg) || CheckFolderExistsAndReadable(defaultMetricsDefinitionPathDocker)) {
		if CheckFolderExistsAndReadable(defaultMetricsDefinitionPathPkg) {
			opts.Metric.MetricsFolder = defaultMetricsDefinitionPathPkg
		} else if CheckFolderExistsAndReadable(defaultMetricsDefinitionPathDocker) {
			opts.Metric.MetricsFolder = defaultMetricsDefinitionPathDocker
		}
		logger.Warningf("--metrics-folder path not specified, using %s", opts.Metric.MetricsFolder)
		fileBasedMetrics = true
	} else { // normal "Config DB" mode
		// make sure all PG params are there
		if opts.Connection.User == "" {
			opts.Connection.User = os.Getenv("USER")
		}
		if opts.Connection.Host == "" || opts.Connection.Port == "" || opts.Connection.Dbname == "" || opts.Connection.User == "" {
			fmt.Println("Check config DB parameters")
			return
		}

		_ = InitAndTestConfigStoreConnection(ctx, opts.Connection.Host,
			opts.Connection.Port, opts.Connection.Dbname, opts.Connection.User, opts.Connection.Password,
			opts.Connection.PgRequireSSL, true)
	}

	pgBouncerNumericCountersStartVersion, _ = decimal.NewFromString("1.12")

	if opts.InternalStatsPort > 0 && !opts.Ping {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", opts.InternalStatsPort))
		if err != nil {
			logger.Fatalf("Could not start the internal statistics interface on port %d. Set --internal-stats-port to an open port or to 0 to disable. Err: %v", opts.InternalStatsPort, err)
		}
		err = l.Close()
		if err != nil {
			logger.Fatalf("Could not cleanly stop the temporary listener on port %d: %v", opts.InternalStatsPort, err)
		}
		logger.Infof("Starting the internal statistics interface on port %d...", opts.InternalStatsPort)
		go StartStatsServer(opts.InternalStatsPort)
		go StatsSummarizer()
	}

	if opts.Metric.PrometheusAsyncMode {
		opts.BatchingDelayMs = 0 // using internal cache, no batching for storage smoothing needed
	}

	controlChannels := make(map[string](chan ControlMessage)) // [db1+metric1]=chan
	persistCh := make(chan []MetricStoreMessage, 10000)
	var bufferedPersistCh chan []MetricStoreMessage

	if !opts.Ping {

		if opts.BatchingDelayMs > 0 && opts.Metric.Datastore != datastorePrometheus {
			bufferedPersistCh = make(chan []MetricStoreMessage, 10000) // "staging area" for metric storage batching, when enabled
			logger.Info("starting MetricsBatcher...")
			go MetricsBatcher(opts.BatchingDelayMs, bufferedPersistCh, persistCh)
		}

		if opts.Metric.Datastore == datastoreGraphite {
			if opts.Metric.GraphiteHost == "" || opts.Metric.GraphitePort == "" {
				logger.Fatal("--graphite-host/port needed!")
			}
			port, _ := strconv.ParseInt(opts.Metric.GraphitePort, 10, 32)
			graphiteHost = opts.Metric.GraphiteHost
			graphitePort = int(port)
			InitGraphiteConnection(graphiteHost, graphitePort)
			logger.Info("starting GraphitePersister...")
			go MetricsPersister(datastoreGraphite, persistCh)
		} else if opts.Metric.Datastore == datastoreJSON {
			if len(opts.Metric.JSONStorageFile) == 0 {
				logger.Fatal("--datastore=json requires --json-storage-file to be set")
			}
			jsonOutFile, err := os.Create(opts.Metric.JSONStorageFile) // test file path writeability
			if err != nil {
				logger.Fatalf("Could not create JSON storage file: %s", err)
			}
			err = jsonOutFile.Close()
			if err != nil {
				logger.Fatal(err)
			}
			logger.Warningf("In JSON output mode. Gathered metrics will be written to \"%s\"...", opts.Metric.JSONStorageFile)
			go MetricsPersister(datastoreJSON, persistCh)
		} else if opts.Metric.Datastore == datastorePostgres {
			if len(opts.Metric.PGMetricStoreConnStr) == 0 {
				logger.Fatal("--datastore=postgres requires --pg-metric-store-conn-str to be set")
			}

			_ = InitAndTestMetricStoreConnection(opts.Metric.PGMetricStoreConnStr, true)

			PGSchemaType = CheckIfPGSchemaInitializedOrFail()

			logger.Info("starting PostgresPersister...")
			go MetricsPersister(datastorePostgres, persistCh)

			logger.Info("starting UniqueDbnamesListingMaintainer...")
			go UniqueDbnamesListingMaintainer(true)

			if opts.Metric.PGRetentionDays > 0 && PGSchemaType != "custom" && opts.TestdataDays == 0 {
				logger.Info("starting old Postgres metrics cleanup job...")
				go OldPostgresMetricsDeleter(opts.Metric.PGRetentionDays, PGSchemaType)
			}

		} else if opts.Metric.Datastore == datastorePrometheus {
			if opts.TestdataDays != 0 || opts.TestdataMultiplier > 0 {
				logger.Fatal("Test data generation mode cannot be used with Prometheus data store")
			}

			if opts.Metric.PrometheusAsyncMode {
				logger.Info("starting Prometheus Cache Persister...")
				go MetricsPersister(datastorePrometheus, persistCh)
			}
			go StartPrometheusExporter()
		} else {
			logger.Fatal("Unknown datastore. Check the --datastore param")
		}

		_, _ = daemon.SdNotify(false, "READY=1") // Notify systemd, does nothing outside of systemd
	}

	firstLoop := true
	mainLoopCount := 0
	var monitoredDbs []MonitoredDatabase
	var lastMetricsRefreshTime int64
	var metrics map[string]map[decimal.Decimal]MetricVersionProperties
	var hostLastKnownStatusInRecovery = make(map[string]bool) // isInRecovery
	var metricConfig map[string]float64                       // set to host.Metrics or host.MetricsStandby (in case optional config defined and in recovery state

	for { //main loop
		hostsToShutDownDueToRoleChange := make(map[string]bool) // hosts went from master to standby and have "only if master" set
		var controlChannelNameList []string
		gatherersShutDown := 0

		if time.Now().Unix()-lastMetricsRefreshTime > metricDefinitionRefreshTime {
			//metrics
			if fileBasedMetrics {
				metrics, err = ReadMetricsFromFolder(opts.Metric.MetricsFolder, firstLoop)
			} else {
				metrics, err = ReadMetricDefinitionMapFromPostgres(firstLoop)
			}
			if err == nil {
				UpdateMetricDefinitionMap(metrics)
				lastMetricsRefreshTime = time.Now().Unix()
			} else {
				logger.Errorf("Could not refresh metric definitions: %s", err)
			}
		}

		if fileBasedMetrics {
			pmc, err := ReadPresetMetricsConfigFromFolder(opts.Metric.MetricsFolder, false)
			if err != nil {
				if firstLoop {
					logger.Fatalf("Could not read preset metric config from \"%s\": %s", path.Join(opts.Metric.MetricsFolder, presetConfigYAMLFile), err)
				} else {
					logger.Errorf("Could not read preset metric config from \"%s\": %s", path.Join(opts.Metric.MetricsFolder, presetConfigYAMLFile), err)
				}
			} else {
				presetMetricDefMap = pmc
				logger.Debugf("Loaded preset metric config: %#v", pmc)
			}

			if opts.IsAdHocMode() {
				adhocconfig, ok := pmc[opts.AdHocConfig]
				if !ok {
					logger.Warningf("Could not find a preset metric config named \"%s\", assuming JSON config...", opts.AdHocConfig)
					adhocconfig, err = jsonTextToMap(opts.AdHocConfig)
					if err != nil {
						logger.Fatalf("Could not parse --adhoc-config(%s): %v", opts.AdHocConfig, err)
					}
				}
				md := MonitoredDatabase{DBUniqueName: opts.AdHocUniqueName, DBType: opts.AdHocDBType, Metrics: adhocconfig, LibPQConnStr: opts.AdHocConnString}
				if opts.AdHocDBType == config.DbTypePg {
					monitoredDbs = []MonitoredDatabase{md}
				} else {
					resolved, err := ResolveDatabasesFromConfigEntry(md)
					if err != nil {
						if firstLoop {
							logger.Fatalf("Failed to resolve DBs for ConnStr \"%s\": %s", opts.AdHocConnString, err)
						} else { // keep previously found list
							logger.Errorf("Failed to resolve DBs for ConnStr \"%s\": %s", opts.AdHocConnString, err)
						}
					} else {
						monitoredDbs = resolved
					}
				}
			} else {
				mc, err := ReadMonitoringConfigFromFileOrFolder(opts.Config)
				if err == nil {
					logger.Debugf("Found %d monitoring config entries", len(mc))
					if len(opts.Metric.Group) > 0 {
						var removedCount int
						mc, removedCount = FilterMonitoredDatabasesByGroup(mc, opts.Metric.Group)
						logger.Infof("Filtered out %d config entries based on --groups=%s", removedCount, opts.Metric.Group)
					}
					monitoredDbs = GetMonitoredDatabasesFromMonitoringConfig(mc)
					logger.Debugf("Found %d databases to monitor from %d config items...", len(monitoredDbs), len(mc))
				} else {
					if firstLoop {
						logger.Fatalf("Could not read/parse monitoring config from path: %s. err: %v", opts.Config, err)
					} else {
						logger.Errorf("Could not read/parse monitoring config from path: %s. using last valid config data. err: %v", opts.Config, err)
					}
					time.Sleep(time.Second * time.Duration(opts.Connection.ServersRefreshLoopSeconds))
					continue
				}
			}
		} else {
			monitoredDbs, err = GetMonitoredDatabasesFromConfigDB()
			if err != nil {
				if firstLoop {
					logger.Fatal("could not fetch active hosts - check config!", err)
				} else {
					logger.Error("could not fetch active hosts, using last valid config data. err:", err)
					time.Sleep(time.Second * time.Duration(opts.Connection.ServersRefreshLoopSeconds))
					continue
				}
			}
		}

		if DoesEmergencyTriggerfileExist() {
			logger.Warningf("Emergency pause triggerfile detected at %s, ignoring currently configured DBs", opts.EmergencyPauseTriggerfile)
			monitoredDbs = make([]MonitoredDatabase, 0)
		}

		UpdateMonitoredDBCache(monitoredDbs)

		if lastMonitoredDBsUpdate.IsZero() || lastMonitoredDBsUpdate.Before(time.Now().Add(-1*time.Second*monitoredDbsDatastoreSyncIntervalSeconds)) {
			monitoredDbsCopy := make([]MonitoredDatabase, len(monitoredDbs))
			copy(monitoredDbsCopy, monitoredDbs)
			if opts.BatchingDelayMs > 0 {
				go SyncMonitoredDBsToDatastore(monitoredDbsCopy, bufferedPersistCh)
			} else {
				go SyncMonitoredDBsToDatastore(monitoredDbsCopy, persistCh)
			}
			lastMonitoredDBsUpdate = time.Now()
		}

		if firstLoop && (len(monitoredDbs) == 0 || len(metricDefinitionMap) == 0) {
			logger.Warningf("host info refreshed, nr. of enabled entries in configuration: %d, nr. of distinct metrics: %d", len(monitoredDbs), len(metricDefinitionMap))
		} else {
			logger.Infof("host info refreshed, nr. of enabled entries in configuration: %d, nr. of distinct metrics: %d", len(monitoredDbs), len(metricDefinitionMap))
		}

		if firstLoop {
			firstLoop = false // only used for failing when 1st config reading fails
		}

		for _, host := range monitoredDbs {
			logger.WithField("database", host.DBUniqueName).
				WithField("metric", host.Metrics).
				WithField("tags", host.CustomTags).
				WithField("config", host.HostConfig).Debug()

			dbUnique := host.DBUniqueName
			dbUniqueOrig := host.DBUniqueNameOrig
			dbType := host.DBType
			metricConfig = host.Metrics
			wasInstancePreviouslyDormant := IsDBDormant(dbUnique)

			if host.PasswordType == "aes-gcm-256" && len(opts.AesGcmKeyphrase) == 0 && len(opts.AesGcmKeyphraseFile) == 0 {
				// Warn if any encrypted hosts found but no keyphrase given
				logger.Warningf("Encrypted password type found for host \"%s\", but no decryption keyphrase specified. Use --aes-gcm-keyphrase or --aes-gcm-keyphrase-file params", dbUnique)
			}

			err := InitSQLConnPoolForMonitoredDBIfNil(host)
			if err != nil {
				logger.Warningf("Could not init SQL connection pool for %s, retrying on next main loop. Err: %v", dbUnique, err)
				continue
			}

			InitPGVersionInfoFetchingLockIfNil(host)

			_, connectFailedSoFar := failedInitialConnectHosts[dbUnique]

			if connectFailedSoFar { // idea is not to spwan any runners before we've successfully pinged the DB
				var err error
				var ver DBVersionMapEntry

				if connectFailedSoFar {
					logger.Infof("retrying to connect to uninitialized DB \"%s\"...", dbUnique)
				} else {
					logger.Infof("new host \"%s\" found, checking connectivity...", dbUnique)
				}

				ver, err = DBGetPGVersion(dbUnique, dbType, true)
				if err != nil {
					logger.Errorf("could not start metric gathering for DB \"%s\" due to connection problem: %s", dbUnique, err)
					if opts.AdHocConnString != "" {
						logger.Errorf("will retry in %ds...", opts.Connection.ServersRefreshLoopSeconds)
					}
					failedInitialConnectHosts[dbUnique] = true
					continue
				}
				logger.Infof("Connect OK. [%s] is on version %s (in recovery: %v)", dbUnique, ver.VersionStr, ver.IsInRecovery)
				if connectFailedSoFar {
					delete(failedInitialConnectHosts, dbUnique)
				}
				if ver.IsInRecovery && host.OnlyIfMaster {
					logger.Infof("[%s] not added to monitoring due to 'master only' property", dbUnique)
					continue
				}
				metricConfig = host.Metrics
				hostLastKnownStatusInRecovery[dbUnique] = ver.IsInRecovery
				if ver.IsInRecovery && len(host.MetricsStandby) > 0 {
					metricConfig = host.MetricsStandby
				}

				if !opts.Ping && (host.IsSuperuser || opts.IsAdHocMode() && opts.AdHocCreateHelpers) && IsPostgresDBType(dbType) && !ver.IsInRecovery {
					if opts.Metric.NoHelperFunctions {
						logger.Infof("[%s] Skipping rollout out helper functions due to the --no-helper-functions flag ...", dbUnique)
					} else {
						logger.Infof("Trying to create helper functions if missing for \"%s\"...", dbUnique)
						_ = TryCreateMetricsFetchingHelpers(dbUnique)
					}
				}

				if !(opts.Ping || (opts.Metric.Datastore == datastorePrometheus && !opts.Metric.PrometheusAsyncMode)) {
					time.Sleep(time.Millisecond * 100) // not to cause a huge load spike when starting the daemon with 100+ monitored DBs
				}
			}

			if IsPostgresDBType(host.DBType) {
				var DBSizeMB int64

				if opts.MinDbSizeMB >= 8 { // an empty DB is a bit less than 8MB
					DBSizeMB, _ = DBGetSizeMB(dbUnique) // ignore errors, i.e. only remove from monitoring when we're certain it's under the threshold
					if DBSizeMB != 0 {
						if DBSizeMB < opts.MinDbSizeMB {
							logger.Infof("[%s] DB will be ignored due to the --min-db-size-mb filter. Current (up to %v cached) DB size = %d MB", dbUnique, dbSizeCachingInterval, DBSizeMB)
							hostsToShutDownDueToRoleChange[dbUnique] = true // for the case when DB size was previosly above the threshold
							SetUndersizedDBState(dbUnique, true)
							continue
						}
						SetUndersizedDBState(dbUnique, false)
					}
				}
				ver, err := DBGetPGVersion(dbUnique, host.DBType, false)
				if err == nil { // ok to ignore error, re-tried on next loop
					lastKnownStatusInRecovery := hostLastKnownStatusInRecovery[dbUnique]
					if ver.IsInRecovery && host.OnlyIfMaster {
						logger.Infof("[%s] to be removed from monitoring due to 'master only' property and status change", dbUnique)
						hostsToShutDownDueToRoleChange[dbUnique] = true
						SetRecoveryIgnoredDBState(dbUnique, true)
						continue
					} else if lastKnownStatusInRecovery != ver.IsInRecovery {
						if ver.IsInRecovery && len(host.MetricsStandby) > 0 {
							logger.Warningf("Switching metrics collection for \"%s\" to standby config...", dbUnique)
							metricConfig = host.MetricsStandby
							hostLastKnownStatusInRecovery[dbUnique] = true
						} else {
							logger.Warningf("Switching metrics collection for \"%s\" to primary config...", dbUnique)
							metricConfig = host.Metrics
							hostLastKnownStatusInRecovery[dbUnique] = false
							SetRecoveryIgnoredDBState(dbUnique, false)
						}
					}
				}

				if wasInstancePreviouslyDormant && !IsDBDormant(dbUnique) {
					RestoreSQLConnPoolLimitsForPreviouslyDormantDB(dbUnique)
				}

				if mainLoopCount == 0 && opts.TryCreateListedExtsIfMissing != "" && !ver.IsInRecovery {
					extsToCreate := strings.Split(opts.TryCreateListedExtsIfMissing, ",")
					extsCreated := TryCreateMissingExtensions(dbUnique, extsToCreate, ver.Extensions)
					logger.Infof("[%s] %d/%d extensions created based on --try-create-listed-exts-if-missing input %v", dbUnique, len(extsCreated), len(extsToCreate), extsCreated)
				}
			}

			if opts.Ping {
				continue // don't launch metric fetching threads
			}

			for metricName := range metricConfig {
				if opts.Metric.Datastore == datastorePrometheus && !opts.Metric.PrometheusAsyncMode {
					continue // normal (non-async, no background fetching) Prom mode means only per-scrape fetching
				}
				metric := metricName
				metricDefOk := false

				if strings.HasPrefix(metric, recoPrefix) {
					metric = recoMetricName
				}
				interval := metricConfig[metric]

				if metric == recoMetricName {
					metricDefOk = true
				} else {
					metricDefMapLock.RLock()
					_, metricDefOk = metricDefinitionMap[metric]
					metricDefMapLock.RUnlock()
				}

				dbMetric := dbUnique + dbMetricJoinStr + metric
				_, chOk := controlChannels[dbMetric]

				if metricDefOk && !chOk { // initialize a new per db/per metric control channel
					if interval > 0 {
						hostMetricIntervalMap[dbMetric] = interval
						logger.Infof("starting gatherer for [%s:%s] with interval %v s", dbUnique, metric, interval)
						controlChannels[dbMetric] = make(chan ControlMessage, 1)
						PromAsyncCacheInitIfRequired(dbUnique, metric)
						if opts.BatchingDelayMs > 0 {
							go MetricGathererLoop(dbUnique, dbUniqueOrig, dbType, metric, metricConfig, controlChannels[dbMetric], bufferedPersistCh)
						} else {
							go MetricGathererLoop(dbUnique, dbUniqueOrig, dbType, metric, metricConfig, controlChannels[dbMetric], persistCh)
						}
					}
				} else if (!metricDefOk && chOk) || interval <= 0 {
					// metric definition files were recently removed or interval set to zero
					logger.Warning("shutting down metric", metric, "for", host.DBUniqueName)
					controlChannels[dbMetric] <- ControlMessage{Action: gathererStatusStop}
					delete(controlChannels, dbMetric)
				} else if !metricDefOk {
					epoch, ok := lastSQLFetchError.Load(metric)
					if !ok || ((time.Now().Unix() - epoch.(int64)) > 3600) { // complain only 1x per hour
						logger.Warningf("metric definition \"%s\" not found for \"%s\"", metric, dbUnique)
						lastSQLFetchError.Store(metric, time.Now().Unix())
					}
				} else {
					// check if interval has changed
					if hostMetricIntervalMap[dbMetric] != interval {
						logger.Warning("sending interval update for", dbUnique, metric)
						controlChannels[dbMetric] <- ControlMessage{Action: gathererStatusStart, Config: metricConfig}
						hostMetricIntervalMap[dbMetric] = interval
					}
				}
			}
		}

		atomic.StoreInt32(&mainLoopInitialized, 1) // to hold off scraping until metric fetching runners have been initialized

		if opts.Ping {
			if len(failedInitialConnectHosts) > 0 {
				logger.Errorf("Could not reach %d configured DB host out of %d", len(failedInitialConnectHosts), len(monitoredDbs))
				os.Exit(len(failedInitialConnectHosts))
			}
			logger.Infof("All configured %d DB hosts were reachable", len(monitoredDbs))
			os.Exit(0)
		}

		if opts.TestdataDays != 0 {
			logger.Info("Waiting for all metrics generation goroutines to stop ...")
			time.Sleep(time.Second * 10) // with that time all different metric fetchers should have started
			testDataGenerationModeWG.Wait()
			for {
				pqlen := len(persistCh)
				if pqlen == 0 {
					if opts.Metric.Datastore == datastorePostgres {
						UniqueDbnamesListingMaintainer(false) // refresh Grafana listing table
					}
					logger.Warning("All generators have exited and data stored. Exit")
					os.Exit(0)
				}
				logger.Infof("Waiting for generated metrics to be stored (%d still in queue) ...", pqlen)
				time.Sleep(time.Second * 1)
			}
		}

		if mainLoopCount == 0 {
			goto MainLoopSleep
		}

		// loop over existing channels and stop workers if DB or metric removed from config
		// or state change makes it uninteresting
		logger.Debug("checking if any workers need to be shut down...")
		controlChannelNameList = ControlChannelsMapToList(controlChannels)

		for _, dbMetric := range controlChannelNameList {
			var currentMetricConfig map[string]float64
			var dbInfo MonitoredDatabase
			var ok, dbRemovedFromConfig bool
			singleMetricDisabled := false
			splits := strings.Split(dbMetric, dbMetricJoinStr)
			db := splits[0]
			metric := splits[1]
			//log.Debugf("Checking if need to shut down worker for [%s:%s]...", db, metric)

			_, wholeDbShutDownDueToRoleChange := hostsToShutDownDueToRoleChange[db]
			if !wholeDbShutDownDueToRoleChange {
				monitoredDbCacheLock.RLock()
				dbInfo, ok = monitoredDbCache[db]
				monitoredDbCacheLock.RUnlock()
				if !ok { // normal removing of DB from config
					dbRemovedFromConfig = true
					logger.Debugf("DB %s removed from config, shutting down all metric worker processes...", db)
				}
			}

			if !(wholeDbShutDownDueToRoleChange || dbRemovedFromConfig) { // maybe some single metric was disabled
				dbPgVersionMapLock.RLock()
				verInfo, ok := dbPgVersionMap[db]
				dbPgVersionMapLock.RUnlock()
				if !ok {
					logger.Warningf("Could not find PG version info for DB %s, skipping shutdown check of metric worker process for %s", db, metric)
					continue
				}

				if verInfo.IsInRecovery && len(dbInfo.MetricsStandby) > 0 {
					currentMetricConfig = dbInfo.MetricsStandby
				} else {
					currentMetricConfig = dbInfo.Metrics
				}

				interval, isMetricActive := currentMetricConfig[metric]
				if !isMetricActive || interval <= 0 {
					singleMetricDisabled = true
				}
			}

			if wholeDbShutDownDueToRoleChange || dbRemovedFromConfig || singleMetricDisabled {
				logger.Infof("shutting down gatherer for [%s:%s] ...", db, metric)
				controlChannels[dbMetric] <- ControlMessage{Action: gathererStatusStop}
				delete(controlChannels, dbMetric)
				logger.Debugf("control channel for [%s:%s] deleted", db, metric)
				gatherersShutDown++
				ClearDBUnreachableStateIfAny(db)
				PurgeMetricsFromPromAsyncCacheIfAny(db, metric)
			}
		}

		if gatherersShutDown > 0 {
			logger.Warningf("sent STOP message to %d gatherers (it might take some minutes for them to stop though)", gatherersShutDown)
		}

		// Destroy conn pools, Prom async cache
		CloseResourcesForRemovedMonitoredDBs(monitoredDbs, prevLoopMonitoredDBs, hostsToShutDownDueToRoleChange)

	MainLoopSleep:
		mainLoopCount++
		prevLoopMonitoredDBs = monitoredDbs

		logger.Debugf("main sleeping %ds...", opts.Connection.ServersRefreshLoopSeconds)
		select {
		case <-time.After(time.Second * time.Duration(opts.Connection.ServersRefreshLoopSeconds)):
			// pass
		case <-ctx.Done():
			return
		}
	}

}
