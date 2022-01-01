package sensors

import (
	"github.com/markusressel/fan2go/internal/configuration"
	"github.com/markusressel/fan2go/internal/ui"
	"os/exec"
	"strings"
	"strconv"
)

type CommandSensor struct {
	Name      string                     `json:"name"`
	Cmd       string                     `json:"cmd"`
	Config    configuration.SensorConfig `json:"configuration"`
	MovingAvg float64                    `json:"moving_avg"`
}

func (sensor CommandSensor) GetId() string {
	return sensor.Config.ID
}

func (sensor CommandSensor) GetConfig() configuration.SensorConfig {
	return sensor.Config
}

func (sensor CommandSensor) GetValue() (float64, error) {
	command := strings.Fields(sensor.Cmd)
	cmd := exec.Command(command[0], command[1:]...)
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	integer, err := strconv.Atoi(strings.TrimSpace(string(output[:])))

	if err != nil {
		ui.Warning("Unable to read int from command: \"%s\" with output \"%s\"", sensor.Cmd, output)
		return 0, nil
	}

	result := float64(integer)
	return result, nil
}

func (sensor CommandSensor) GetMovingAvg() (avg float64) {
	return sensor.MovingAvg
}

func (sensor *CommandSensor) SetMovingAvg(avg float64) {
	sensor.MovingAvg = avg
}
