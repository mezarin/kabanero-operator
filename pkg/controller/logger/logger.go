package logger

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var loggerlog = logf.Log.WithName("controller.logger.logger")

type OperatorLogger struct {
	ComponentLoggers map[string]Componentlogger
	TraceComponents  map[string]Level
	TraceSpec        string
}

type Componentlogger struct {
	Logger        logr.Logger
	ComponentName string
}

const (
	// trace spec constants
	COMP_SEPARATOR       = ":"
	COMP_LEVEL_SEPARATOR = "="

	// Configmap constants.
	TRACE_CONFIG_MAP_NAME   = "kabanero-operator-trace"
	LOGGING_DATA_TRACE_SPEC = "tracespec"
)

type Level int

const (
	ERROR_LEVEL   Level = iota + 1 // 1
	WARNING_LEVEL                  // 2
	INFO_LEVEL                     // 3
)

// Returns a string representation of the custom level.
func (l *Level) toString() string {
	switch *l {
	case ERROR_LEVEL:
		return "error"
	case WARNING_LEVEL:
		return "warning"
	case INFO_LEVEL:
		return "info"
	default:
		return fmt.Sprintf("Invalid log level: %v", l)
	}
}

// Retruns a custom Level object from the string input.
func toLevel(l string) Level {
	switch l {
	case "error":
		return ERROR_LEVEL
	case "warning":
		return WARNING_LEVEL
	case "info":
		return INFO_LEVEL
	default:
		return INFO_LEVEL
	}
}

var OLogger OperatorLogger = OperatorLogger{ComponentLoggers: make(map[string]Componentlogger), TraceComponents: make(map[string]Level)}

// Registers new components and returns a new logger instance.
func NewOperatorlogger(componentName string) Componentlogger {
	logger := logf.Log.WithName(componentName)
	ol := Componentlogger{Logger: logger, ComponentName: componentName}
	OLogger.ComponentLoggers[componentName] = ol
	return ol
}

func (l *Componentlogger) Info(msg string, kv ...interface{}) {
	if l.DoLog(INFO_LEVEL) {
		if kv == nil || len(kv) == 0 {
			l.Logger.Info(msg)
		} else {
			l.Logger.Info(msg, kv)
		}
	}
}

func (l *Componentlogger) Warning(msg string, kv ...interface{}) {
	if l.DoLog(WARNING_LEVEL) {
		if kv == nil {
			l.Logger.V(-1 * int(zapcore.WarnLevel)).Info(msg)
		} else {
			l.Logger.V(-1*int(zapcore.WarnLevel)).Info(msg, kv)
		}
	}
}

func (l *Componentlogger) Error(err error, msg string, kv ...interface{}) {
	if l.DoLog(ERROR_LEVEL) {
		if kv == nil {
			l.Logger.Error(err, msg)
		} else {
			l.Logger.Error(err, msg, kv)
		}
	}
}

// Sets the components that will be traced. When specifying a component the following format applies:
// <component>=<level>:<component>=<level>...
// Where:
// a. <component> is the name of the component used when calling NewOperatorlogger(... componentName string... )
// b. <level> is one of the followings: all, error, warning, info, debug. If no level is set to info.
//
// Currently supportted definitions:
// a. componentA=info:componentB:warning:componentC:warning
// b. *=warning
func SetTraceComponents(c client.Client, r client.Reader, namespace string, loggingData map[string]string) {
	var data map[string]string
	if loggingData == nil || len(loggingData) == 0 {
		cm := &v1.ConfigMap{}
		var err error
		if c != nil {
			err = c.Get(context.Background(), types.NamespacedName{Name: TRACE_CONFIG_MAP_NAME, Namespace: namespace}, cm)
		} else {
			err = r.Get(context.Background(), types.NamespacedName{Name: TRACE_CONFIG_MAP_NAME, Namespace: namespace}, cm)
		}
		if err != nil {
			if errors.IsNotFound(err) {
				loggerlog.Info(fmt.Sprintf("Trace config map was not found. The default traces spec is set. ConfigMapName: %v. Namespace: %v. Error: %v", TRACE_CONFIG_MAP_NAME, namespace, err))
			} else {
				loggerlog.V(-1 * int(zapcore.WarnLevel)).Info(fmt.Sprintf("An error was encountered while retrieving trace config map. The default trace spec is used. ConfigMapName: %v. Namepsace: %v. Error: %v", TRACE_CONFIG_MAP_NAME, namespace, err))
			}
		} else {
			data = cm.Data
		}
	} else {
		data = loggingData
	}

	setTraceComponentsWithData(data)

}

// Processes the trace config input data.
func setTraceComponentsWithData(loggingData map[string]string) {
	var ts string
	if loggingData != nil {
		ts, _ = loggingData[LOGGING_DATA_TRACE_SPEC]
	}

	// If nothing is found, set the default info level for all components.
	if len(ts) == 0 {
		loggerlog.Info("No trace spec found. Info level is used.")
		ts = "*=info"
	}
	// If there is a single component entry improperly formatted set the default info level for all components.
	if !strings.Contains(ts, COMP_SEPARATOR) && !strings.Contains(ts, COMP_LEVEL_SEPARATOR) {
		loggerlog.Info(fmt.Sprintf("Trace spec %v is improperly formatted. Info level is used.", ts))
		ts = "*=info"
	}

	// Parse component traces. If an unknown trace level was specified for a specific component,
	// the info level default is used. If a component entry is not properly formated, the trace entry
	// is ignored.
	comps := make(map[string]Level)
	for _, comp := range strings.Split(ts, COMP_SEPARATOR) {
		if strings.Contains(comp, COMP_LEVEL_SEPARATOR) {
			compAndLevel := strings.Split(comp, COMP_LEVEL_SEPARATOR)
			comps[compAndLevel[0]] = toLevel(compAndLevel[1])
		}
	}

	if OLogger.TraceSpec != ts {
		loggerlog.Info(fmt.Sprintf("Configured trace level specification: %v", ts))
	}

	OLogger.TraceSpec = ts
	OLogger.TraceComponents = comps
}

// Returns true if the trace is to be logged. False otherwise.
func (l *Componentlogger) DoLog(traceLevelToLog Level) bool {
	allowedTraceLevel, found := OLogger.TraceComponents["*"]
	if !found {
		allowedTraceLevel, found = OLogger.TraceComponents[l.ComponentName]
		if !found {
			return false
		}
	}

	if traceLevelToLog <= allowedTraceLevel {
		return true
	}

	return false
}

// Creates and/or updates a trace config map.
func GetTraceConfigMap(c client.Client, namespace string, data map[string]string) (*v1.ConfigMap, bool, error) {
	created := false
	cm := &v1.ConfigMap{}
	err := c.Get(context.TODO(), types.NamespacedName{Name: TRACE_CONFIG_MAP_NAME, Namespace: namespace}, cm)

	if err != nil {
		if errors.IsNotFound(err) {
			cm = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      TRACE_CONFIG_MAP_NAME,
				},
			}
			created = true
		} else {
			return nil, created, err
		}
	}

	cm.Data = data

	return cm, created, nil
}

// Returns a new SharedIndexInformer that handles configuration updates associated with the
// config map holding config data.
func GetTraceConfigmapInformer(namespace string) (cache.SharedIndexInformer, error) {
	kubeCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(kubeCfg)
	if err != nil {
		return nil, err
	}

	watchlist := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		string(v1.ResourceConfigMaps),
		namespace,
		fields.Everything(),
	)

	informer := cache.NewSharedIndexInformer(watchlist, &v1.ConfigMap{}, 60*time.Second, nil)
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, obj interface{}) {
			ccm := obj.(*v1.ConfigMap)
			ocm := old.(*v1.ConfigMap)
			if ccm.Name != TRACE_CONFIG_MAP_NAME || ocm.GetResourceVersion() == ccm.GetResourceVersion() {
				return
			}
			setTraceComponentsWithData(ccm.Data)
		},
	})

	return informer, nil
}
