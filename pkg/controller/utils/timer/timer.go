package timer

import (
	"fmt"
	"time"

	ologger "github.com/kabanero-io/kabanero-operator/pkg/controller/logger"
)

var utlog = ologger.NewOperatorlogger("controller.utils.timer.timer")

// GenFunc is a generic retriable function
type GenRetryFunc func() (bool, error)

// Retry executes the given function for the specified number of retry attempts.
func Retry(attempts int, waitTime time.Duration, gf GenRetryFunc) error {
	for i := 0; i < attempts; i++ {
		ok, err := gf()
		if err != nil {
			return err
		}

		if ok {
			return nil
		}

		time.Sleep(waitTime)
	}

	return fmt.Errorf("Retriable function did not reach the expected outcome. Retry attempts: %v. Wait time: %v", attempts, waitTime)
}

// GenSchedFunc is a generic scheduleable function
type GenSchedFunc func(timeparm time.Duration)

// Starts ticker task to run custom work.
func ScheduleWork(tickerDuration time.Duration, gsf GenSchedFunc, timeparm time.Duration) {
	// Start a ticker that will receive periodic requests to run the input function.
	purgeTicker := time.NewTicker(tickerDuration)

	// This is the function that will run custom work.  Note that this function
	// never ends since we expect this to be running in a Kubernetes pod which will
	// never end on its own.
	go func() {
		for {
			select {
			case <-purgeTicker.C:
				utlog.Info("Started execution of scheduled custom work.")
				gsf(timeparm)
				utlog.Info("Finished execution of scheduled custom work.")
			}
		}
	}()
}
