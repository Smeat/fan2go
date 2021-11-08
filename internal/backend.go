package internal

import (
	"context"
	"errors"
	"fmt"
	"github.com/asecurityteam/rolling"
	"github.com/markusressel/fan2go/internal/configuration"
	"github.com/markusressel/fan2go/internal/sensors"
	"github.com/markusressel/fan2go/internal/ui"
	"github.com/markusressel/fan2go/internal/util"
	"github.com/oklog/run"
	bolt "go.etcd.io/bbolt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	MaxPwmValue       = 255
	MinPwmValue       = 0
	InitialLastSetPwm = -10
)

var (
	InitializationSequenceMutex sync.Mutex
	SensorMap                   = map[string]Sensor{}
	CurveMap                    = map[string]*configuration.CurveConfig{}
	Verbose                     bool
)

func Run(verbose bool) {
	Verbose = verbose
	if getProcessOwner() != "root" {
		ui.Fatal("Fan control requires root permissions to be able to modify fan speeds, please run fan2go as root")
	}

	db := OpenPersistence(configuration.CurrentConfig.DbPath)
	defer db.Close()

	controllers, err := FindControllers()
	if err != nil {
		ui.Fatal("Error detecting devices: %s", err.Error())
	}
	mapConfigToControllers(controllers)
	for _, curveConfig := range configuration.CurrentConfig.Curves {
		var config = curveConfig
		CurveMap[curveConfig.Id] = &config
	}

	ctx, cancel := context.WithCancel(context.Background())

	var g run.Group
	{
		// === sensor monitoring
		tempTick := time.Tick(configuration.CurrentConfig.TempSensorPollingRate)

		for _, controller := range controllers {
			for _, s := range controller.Sensors {
				if s.GetConfig() == nil {
					ui.Info("Ignoring unconfigured sensor %s/%s", controller.Name, s.GetLabel())
					continue
				}

				sensorId := s.GetConfig().Id

				g.Add(func() error {
					return sensorMonitor(ctx, sensorId, tempTick)
				}, func(err error) {
					ui.Fatal("Error monitoring sensor: %v", err)
				})
			}
		}
	}
	{
		// === fan controllers
		count := 0
		for _, controller := range controllers {
			for _, f := range controller.Fans {
				fan := f
				if fan.Config == nil {
					// this fan is not configured, ignore it
					ui.Info("Ignoring unconfigured fan %s/%s (%s)", controller.Name, fan.Name, fan.Label)
					continue
				}

				g.Add(func() error {
					rpmTick := time.Tick(configuration.CurrentConfig.RpmPollingRate)
					return rpmMonitor(ctx, fan, rpmTick)
				}, func(err error) {
					// nothing to do here
				})

				g.Add(func() error {
					ui.Info("Gathering sensor data for %s...", fan.Config.Id)
					// wait a bit to gather monitoring data
					time.Sleep(2*time.Second + configuration.CurrentConfig.TempSensorPollingRate*2)

					tick := time.Tick(configuration.CurrentConfig.ControllerAdjustmentTickRate)
					return fanController(ctx, db, fan, tick)
				}, func(err error) {
					if err != nil {
						ui.Error("Something went wrong: %v", err)
					}

					ui.Info("Trying to restore fan settings for %s...", fan.Config.Id)

					// try to reset the pwm_enable value
					if fan.OriginalPwmEnabled != 1 {
						err := setPwmEnabled(fan, fan.OriginalPwmEnabled)
						if err == nil {
							return
						}
					}
					err = setPwm(fan, MaxPwmValue)
					if err != nil {
						ui.Warning("Unable to restore fan %s, make sure it is running!", fan.Config.Id)
					}
				})
				count++
			}
		}

		if count == 0 {
			ui.Fatal("No valid fan configurations, exiting.")
		}
	}
	{
		sig := make(chan os.Signal)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, os.Kill)

		g.Add(func() error {
			<-sig
			ui.Info("Exiting...")
			return nil
		}, func(err error) {
			cancel()
			close(sig)
		})
	}

	if err := g.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rpmMonitor(ctx context.Context, fan *Fan, tick <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			measureRpm(fan)
		}
	}
}

func sensorMonitor(ctx context.Context, sensorId string, tick <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			err := updateSensor(sensorId)
			if err != nil {
				ui.Fatal("%v", err)
			}
		}
	}
}

func getProcessOwner() string {
	stdout, err := exec.Command("ps", "-o", "user=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		ui.Error("%v", err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(stdout))
}

// Map detect devices to configuration values
func mapConfigToControllers(controllers []*Controller) {
	for _, controller := range controllers {
		// match fan and fan config entries
		for _, fan := range controller.Fans {
			fanConfig := findFanConfig(controller, fan)
			if fanConfig != nil {
				if Verbose {
					ui.Debug("Mapping fan config %s to %s", fanConfig.Id, fan.PwmOutput)
				}
				fan.Config = fanConfig
			}
		}
		// match sensor and sensor config entries
		for _, s := range controller.Sensors {
			sensorConfig := findSensorConfig(controller, s.(*sensors.HwmonSensor))
			if sensorConfig != nil {
				if Verbose {
					ui.Debug("Mapping sensor config %s to %s", sensorConfig.Id, s.(*sensors.HwmonSensor).Input)
				}

				s.SetConfig(sensorConfig)

				// remember ID -> Sensor association for later
				SensorMap[sensorConfig.Id] = s

				// initialize arrays for storing temps
				currentValue, err := s.GetValue()
				if err != nil {
					ui.Fatal("Error reading sensor %s: %s", sensorConfig.Id, err.Error())
				}
				s.SetMovingAvg(currentValue)
			}
		}
	}
}

// read the current value of a fan RPM sensor and append it to the moving window
func measureRpm(fan *Fan) {
	pwm := GetPwm(fan)
	rpm := GetRpm(fan)

	if Verbose {
		ui.Debug("Measured RPM of %d at PWM %d for fan %s", rpm, pwm, fan.Config.Id)
	}

	fan.RpmMovingAvg = updateSimpleMovingAvg(fan.RpmMovingAvg, configuration.CurrentConfig.RpmRollingWindowSize, float64(rpm))

	pwmRpmMap := fan.FanCurveData
	pointWindow, exists := (*pwmRpmMap)[pwm]
	if !exists {
		// create rolling window for current pwm value
		pointWindow = createRollingWindow(configuration.CurrentConfig.RpmRollingWindowSize)
		(*pwmRpmMap)[pwm] = pointWindow
	}
	pointWindow.Append(float64(rpm))
}

// GetPwmBoundaries calculates the startPwm and maxPwm values for a fan based on its fan curve data
func GetPwmBoundaries(fan *Fan) (startPwm int, maxPwm int) {
	startPwm = 255
	maxPwm = 255
	pwmRpmMap := fan.FanCurveData

	// get pwm keys that we have data for
	keys := make([]int, len(*pwmRpmMap))
	if pwmRpmMap == nil || len(keys) <= 0 {
		// we have no data yet
		startPwm = 0
	} else {
		i := 0
		for k := range *pwmRpmMap {
			keys[i] = k
			i++
		}
		// sort them increasing
		sort.Ints(keys)

		maxRpm := 0
		for _, pwm := range keys {
			window := (*pwmRpmMap)[pwm]
			avgRpm := int(getWindowAvg(window))

			if avgRpm > maxRpm {
				maxRpm = avgRpm
				maxPwm = pwm
			}

			if avgRpm > 0 && pwm < startPwm {
				startPwm = pwm
			}
		}
	}

	return startPwm, maxPwm
}

// read the current value of a sensors and append it to the moving window
func updateSensor(sensorId string) (err error) {
	s := SensorMap[sensorId]

	value, err := s.GetValue()
	if err != nil {
		return err
	}

	var n = configuration.CurrentConfig.TempRollingWindowSize
	lastAvg := s.GetMovingAvg()
	newAvg := updateSimpleMovingAvg(lastAvg, n, value)
	s.SetMovingAvg(newAvg)

	return nil
}

// goroutine to continuously adjust the speed of a fan
func fanController(ctx context.Context, db *bolt.DB, fan *Fan, tick <-chan time.Time) (err error) {
	// check if we have data for this fan in persistence,
	// if not we need to run the initialization sequence
	ui.Info("Loading fan curve data for fan '%s'...", fan.Config.Id)
	fanPwmData, err := LoadFanPwmData(db, fan)
	if err != nil {
		ui.Warning("No fan curve data found for fan '%s', starting initialization sequence...", fan.Config.Id)
		err = runInitializationSequence(db, fan)
		if err != nil {
			return err
		}
	}

	fanPwmData, err = LoadFanPwmData(db, fan)
	if err != nil {
		return err
	}

	err = AttachFanCurveData(&fanPwmData, fan)
	if err != nil {
		return err
	}

	ui.Info("Start PWM of %s (%s, %s): %d", fan.Config.Id, fan.Label, fan.Name, fan.MinPwm)
	ui.Info("Max PWM of %s (%s, %s): %d", fan.Config.Id, fan.Label, fan.Name, fan.MaxPwm)

	err = trySetManualPwm(fan)
	if err != nil {
		ui.Error("Could not enable fan control on %s (%s, %s)", fan.Config.Id, fan.Label, fan.Name)
		return err
	}

	ui.Info("Starting controller loop for fan '%s' (%s, %s)", fan.Config.Id, fan.Label, fan.Name)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			current := GetPwm(fan)
			optimalPwm, err := calculateOptimalPwm(fan)
			if err != nil {
				ui.Error("Unable to calculate optimal PWM value for %s (%s, %s): %v", fan.Config.Id, fan.Label, fan.Name, err)
				return err
			}
			target := calculateTargetPwm(fan, current, optimalPwm)
			err = setPwm(fan, target)
			if err != nil {
				ui.Error("Error setting %s (%s, %s): %v", fan.Config.Id, fan.Label, fan.Name, err)
				err = trySetManualPwm(fan)
				if err != nil {
					ui.Error("Could not enable fan control on %s (%s, %s)", fan.Config.Id, fan.Label, fan.Name)
					return err
				}
			}
		}
	}
}

// AttachFanCurveData attaches fan curve data from persistence to a fan
// Note: When the given data is incomplete, all values up until the highest
// value in the given dataset will be interpolated linearly
// returns os.ErrInvalid if curveData is void of any data
func AttachFanCurveData(curveData *map[int][]float64, fan *Fan) (err error) {
	// convert the persisted map to arrays back to a moving window and attach it to the fan

	if curveData == nil || len(*curveData) <= 0 {
		ui.Error("Cant attach empty fan curve data to fan %s, %s", fan.Label, fan.Name)
		return os.ErrInvalid
	}

	const limit = 255
	var lastValueIdx int
	var lastValueAvg float64
	var nextValueIdx int
	var nextValueAvg float64
	for i := 0; i <= limit; i++ {
		fanCurveMovingWindow := createRollingWindow(configuration.CurrentConfig.RpmRollingWindowSize)

		pointValues, containsKey := (*curveData)[i]
		if containsKey && len(pointValues) > 0 {
			lastValueIdx = i
			lastValueAvg = util.Avg(pointValues)
		} else {
			if pointValues == nil {
				pointValues = []float64{lastValueAvg}
			}
			// find next value in curveData
			nextValueIdx = i
			for j := i; j <= limit; j++ {
				pointValues, containsKey := (*curveData)[j]
				if containsKey {
					nextValueIdx = j
					nextValueAvg = util.Avg(pointValues)
				}
			}
			if nextValueIdx == i {
				// we didn't find a next value in curveData, so we just copy the last point
				var valuesCopy = []float64{}
				copy(pointValues, valuesCopy)
				pointValues = valuesCopy
			} else {
				// interpolate average value to the next existing key
				ratio := util.Ratio(float64(i), float64(lastValueIdx), float64(nextValueIdx))
				interpolation := lastValueAvg + ratio*(nextValueAvg-lastValueAvg)
				pointValues = []float64{interpolation}
			}
		}

		var currentAvg float64
		for k := 0; k < configuration.CurrentConfig.RpmRollingWindowSize; k++ {
			var rpm float64

			if k < len(pointValues) {
				rpm = pointValues[k]
			} else {
				// fill the rolling window with averages if given values are not sufficient
				rpm = currentAvg
			}

			// update average
			if k == 0 {
				currentAvg = rpm
			} else {
				currentAvg = (currentAvg + rpm) / 2
			}

			// add value to window
			fanCurveMovingWindow.Append(rpm)
		}

		(*fan.FanCurveData)[i] = fanCurveMovingWindow
	}

	fan.StartPwm, fan.MaxPwm = GetPwmBoundaries(fan)
	// TODO: we don't have a way to determine this yet
	fan.MinPwm = fan.StartPwm

	return err
}

func trySetManualPwm(fan *Fan) (err error) {
	err = setPwmEnabled(fan, 1)
	if err != nil {
		err = setPwmEnabled(fan, 0)
	}
	return err
}

// runs an initialization sequence for the given fan
// to determine an estimation of its fan curve
func runInitializationSequence(db *bolt.DB, fan *Fan) (err error) {
	if configuration.CurrentConfig.RunFanInitializationInParallel == false {
		InitializationSequenceMutex.Lock()
		defer InitializationSequenceMutex.Unlock()
	}

	err = trySetManualPwm(fan)
	if err != nil {
		ui.Error("Could not enable fan control on %s (%s, %s)", fan.Config.Id, fan.Label, fan.Name)
		return err
	}

	for pwm := 0; pwm <= MaxPwmValue; pwm++ {
		// set a pwm
		err = util.WriteIntToFile(pwm, fan.PwmOutput)
		if err != nil {
			ui.Error("Unable to run initialization sequence on %s (%s, %s): %v", fan.Config.Id, fan.Label, fan.Name, err)
			return err
		}

		if pwm == 0 {
			// TODO: this "waiting" logic could also be applied to the other measurements
			diffThreshold := configuration.CurrentConfig.MaxRpmDiffForSettledFan

			measuredRpmDiffWindow := createRollingWindow(10)
			fillWindow(measuredRpmDiffWindow, 10, 2*diffThreshold)
			measuredRpmDiffMax := 2 * diffThreshold
			oldRpm := 0
			for !(measuredRpmDiffMax < diffThreshold) {
				ui.Debug("Waiting for fan %s (%s, %s) to settle (current RPM max diff: %f)...", fan.Config.Id, fan.Label, fan.Name, measuredRpmDiffMax)
				currentRpm := GetRpm(fan)
				measuredRpmDiffWindow.Append(math.Abs(float64(currentRpm - oldRpm)))
				oldRpm = currentRpm
				measuredRpmDiffMax = math.Ceil(getWindowMax(measuredRpmDiffWindow))
				time.Sleep(1 * time.Second)
			}
			ui.Debug("Fan %s (%s, %s) has settled (current RPM max diff: %f)", fan.Config.Id, fan.Label, fan.Name, measuredRpmDiffMax)
		} else {
			// wait a bit to allow the fan speed to settle.
			// since most sensors are update only each second,
			// we wait double that to make sure we get
			// the most recent measurement
			time.Sleep(2 * time.Second)
		}

		// TODO:
		// on some fans it is not possible to use the full pwm of 0..255
		// so we try what values work and save them for later

		ui.Debug("Measuring RPM of %s (%s, %s) at PWM: %d", fan.Config.Id, fan.Label, fan.Name, pwm)
		for i := 0; i < configuration.CurrentConfig.RpmRollingWindowSize; i++ {
			// update rpm curve
			measureRpm(fan)
		}
	}

	// save to database to restore it on restarts
	err = SaveFanPwmData(db, fan)
	if err != nil {
		ui.Error("Failed to save fan PWM data for %s: %v", fan.Config.Id, err)
	}
	return err
}

func findFanConfig(controller *Controller, fan *Fan) (fanConfig *configuration.FanConfig) {
	for _, fanConfig := range configuration.CurrentConfig.Fans {
		if controller.Platform == fanConfig.Platform &&
			fan.Index == fanConfig.Fan {
			return &fanConfig
		}
	}
	return nil
}

func findSensorConfig(controller *Controller, sensor Sensor) (sensorConfig *configuration.SensorConfig) {
	for _, sensorConfig := range configuration.CurrentConfig.Sensors {
		if controller.Platform == sensorConfig.Platform &&
			sensor.(*sensors.HwmonSensor).Index == sensorConfig.Index {
			return &sensorConfig
		}
	}
	return nil
}

// FindControllers Finds controllers and fans
func FindControllers() (controllers []*Controller, err error) {
	hwmonDevices := util.FindHwmonDevicePaths()
	i2cDevices := util.FindI2cDevicePaths()
	allDevices := append(hwmonDevices, i2cDevices...)

	platformRegex := regexp.MustCompile(".*/platform/{}/.*")
	pciDeviceRegex := regexp.MustCompile("\\d{4}:\\d{2}:\\d{2}\\.\\d")

	for _, devicePath := range allDevices {

		var name = util.GetDeviceName(devicePath)
		if strings.Contains(devicePath, "/pci") {
			// add pci suffix to name
			matches := pciDeviceRegex.FindAllString(devicePath, -1)
			lastMatch := matches[len(matches)-1]
			pciIdentifier := util.CreateShortPciIdentifier(lastMatch)
			name = fmt.Sprintf("%s-%s", name, pciIdentifier)
		}

		dType := util.GetDeviceType(devicePath)
		modalias := util.GetDeviceModalias(devicePath)
		platform := platformRegex.FindString(devicePath)
		if len(platform) <= 0 {
			platform = name
		}

		fans := createFans(devicePath)
		sensorList := createSensors(devicePath)

		if len(fans) <= 0 && len(sensorList) <= 0 {
			continue
		}

		controller := Controller{
			Name:     name,
			DType:    dType,
			Modalias: modalias,
			Platform: platform,
			Path:     devicePath,
			Fans:     fans,
			Sensors:  sensorList,
		}
		controllers = append(controllers, &controller)
	}

	return controllers, err
}

// creates fan objects for the given device path
func createFans(devicePath string) (fans []*Fan) {
	inputs := util.FindFilesMatching(devicePath, "^fan[1-9]_input$")
	outputs := util.FindFilesMatching(devicePath, "^pwm[1-9]$")

	for idx, output := range outputs {
		_, file := filepath.Split(output)

		label := util.GetLabel(devicePath, output)

		index, err := strconv.Atoi(file[len(file)-1:])
		if err != nil {
			ui.Fatal("%v", err)
		}

		fan := &Fan{
			Name:         file,
			Label:        label,
			Index:        index,
			PwmOutput:    output,
			RpmInput:     inputs[idx],
			RpmMovingAvg: 0,
			MinPwm:       MinPwmValue,
			MaxPwm:       MaxPwmValue,
			FanCurveData: &map[int]*rolling.PointPolicy{},
			LastSetPwm:   InitialLastSetPwm,
		}

		// store original pwm_enable value
		pwmEnabled, err := getPwmEnabled(fan)
		if err != nil {
			ui.Fatal("Cannot read pwm_enable value of %s", fan.Config.Id)
		}
		fan.OriginalPwmEnabled = pwmEnabled

		fans = append(fans, fan)
	}

	return fans
}

// creates sensor objects for the given device path
func createSensors(devicePath string) (result []Sensor) {
	inputs := util.FindFilesMatching(devicePath, "^temp[1-9]_input$")

	for _, input := range inputs {
		_, file := filepath.Split(input)
		label := util.GetLabel(devicePath, file)

		index, err := strconv.Atoi(string(file[4]))
		if err != nil {
			ui.Fatal("%v", err)
		}

		sensor := &sensors.HwmonSensor{
			Name:  file,
			Label: label,
			Index: index,
			Input: input,
		}
		result = append(result, sensor)
	}

	return result
}

// IsPwmAuto checks if the given output is in auto mode
func IsPwmAuto(outputPath string) (bool, error) {
	pwmEnabledFilePath := outputPath + "_enable"

	if _, err := os.Stat(pwmEnabledFilePath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		panic(err)
	}

	value, err := util.ReadIntFromFile(pwmEnabledFilePath)
	if err != nil {
		return false, err
	}
	return value > 1, nil
}

// Writes the given value to pwmX_enable
// Possible values (unsure if these are true for all scenarios):
// 0 - no control (results in max speed)
// 1 - manual pwm control
// 2 - motherboard pwm control
func setPwmEnabled(fan *Fan, value int) (err error) {
	pwmEnabledFilePath := fan.PwmOutput + "_enable"
	err = util.WriteIntToFile(value, pwmEnabledFilePath)
	if err == nil {
		value, err := util.ReadIntFromFile(pwmEnabledFilePath)
		if err != nil || value != value {
			return errors.New(fmt.Sprintf("PWM mode stuck to %d", value))
		}
	}
	return err
}

// get the pwmX_enable value of a fan
func getPwmEnabled(fan *Fan) (int, error) {
	pwmEnabledFilePath := fan.PwmOutput + "_enable"
	return util.ReadIntFromFile(pwmEnabledFilePath)
}

// get the maximum valid pwm value of a fan
func getMaxPwmValue(fan *Fan) (result int) {
	return fan.MaxPwm
}

// get the minimum valid pwm value of a fan
func getMinPwmValue(fan *Fan) (result int) {
	// if the fan is never supposed to stop,
	// use the lowest pwm value where the fan is still spinning
	if fan.Config.NeverStop {
		return fan.MinPwm
	}

	return MinPwmValue
}

// GetPwm get the pwm speed of a fan (0..255)
func GetPwm(fan *Fan) (value int) {
	value, err := util.ReadIntFromFile(fan.PwmOutput)
	if err != nil {
		value = MinPwmValue
	}
	return value
}

// calculates the target speed for a given device output
func calculateOptimalPwm(fan *Fan) (int, error) {
	curve := CurveMap[fan.Config.Curve]
	return evaluateCurve(*curve)
}

// calculates the optimal pwm for a fan with the given target
// level.
// returns -1 if no rpm is detected even at fan.maxPwm
func calculateTargetPwm(fan *Fan, currentPwm int, pwm int) int {
	target := pwm

	// ensure target value is within bounds of possible values
	if target > MaxPwmValue {
		ui.Warning("Tried to set out-of-bounds PWM value %d on fan %s", pwm, fan.Config.Id)
		target = MaxPwmValue
	} else if target < MinPwmValue {
		ui.Warning("Tried to set out-of-bounds PWM value %d on fan %s", pwm, fan.Config.Id)
		target = MinPwmValue
	}

	// map the target value to the possible range of this fan
	maxPwm := getMaxPwmValue(fan)
	minPwm := getMinPwmValue(fan)

	// TODO: this assumes a linear curve, but it might be something else
	target = minPwm + int((float64(target)/MaxPwmValue)*(float64(maxPwm)-float64(minPwm)))

	if fan.LastSetPwm != InitialLastSetPwm && fan.LastSetPwm != currentPwm {
		ui.Warning("PWM of %s was changed by third party! Last set PWM value was: %d but is now: %d",
			fan.Config.Id, fan.LastSetPwm, currentPwm)
	}

	// make sure fans never stop by validating the current RPM
	// and adjusting the target PWM value upwards if necessary
	if fan.Config.NeverStop && fan.LastSetPwm == target {
		avgRpm := fan.RpmMovingAvg
		if avgRpm <= 0 {
			if target >= maxPwm {
				ui.Error("CRITICAL: Fan avg. RPM is %f, even at PWM value %d", avgRpm, target)
				return -1
			}
			ui.Warning("WARNING: Increasing startPWM of %s from %d to %d, which is supposed to never stop, but RPM is %f", fan.Config.Id, fan.MinPwm, fan.MinPwm+1, avgRpm)
			fan.MinPwm++
			target++

			// set the moving avg to a value > 0 to prevent
			// this increase from happening too fast
			fan.RpmMovingAvg = 1
		}
	}

	return target
}

// set the pwm speed of a fan to the specified value (0..255)
func setPwm(fan *Fan, target int) (err error) {
	current := GetPwm(fan)
	if target == current {
		return nil
	}
	if Verbose {
		ui.Debug("Setting %s (%s, %s) to %d ...", fan.Config.Id, fan.Label, fan.Name, target)
	}
	err = util.WriteIntToFile(target, fan.PwmOutput)
	if err == nil {
		fan.LastSetPwm = target
	}
	return err
}

// GetRpm get the rpm value of a fan
func GetRpm(fan *Fan) (value int) {
	value, err := util.ReadIntFromFile(fan.RpmInput)
	if err != nil {
		value = -1
	}
	return value
}

// calculates the new moving average, based on an existing average and buffer size
func updateSimpleMovingAvg(oldAvg float64, n int, newValue float64) float64 {
	return oldAvg + (1/float64(n))*(newValue-oldAvg)
}

func createRollingWindow(size int) *rolling.PointPolicy {
	return rolling.NewPointPolicy(rolling.NewWindow(size))
}

// completely fills the given window with the given value
func fillWindow(window *rolling.PointPolicy, size int, value float64) {
	for i := 0; i < size; i++ {
		window.Append(value)
	}
}

// returns the max value in the window
func getWindowMax(window *rolling.PointPolicy) float64 {
	return window.Reduce(rolling.Max)
}

// returns the average of all values in the window
func getWindowAvg(window *rolling.PointPolicy) float64 {
	return window.Reduce(rolling.Avg)
}
