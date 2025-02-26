package sensor

import (
	"errors"
	"fmt"
	"github.com/markusressel/fan2go/internal/configuration"
	"github.com/markusressel/fan2go/internal/hwmon"
	"github.com/markusressel/fan2go/internal/sensors"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"regexp"
)

var sensorId string

var Command = &cobra.Command{
	Use:              "sensor",
	Short:            "sensor related commands",
	Long:             ``,
	TraverseChildren: true,
	Args:             cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		pterm.DisableOutput()

		sensorIdFlag := cmd.Flag("id")
		sensorId := sensorIdFlag.Value.String()

		sensor, err := getSensor(sensorId)
		if err != nil {
			return err
		}

		value, err := sensor.GetValue()
		if err != nil {
			return err
		}
		fmt.Printf("%d", int(value))
		return nil
	},
}

func init() {
	Command.PersistentFlags().StringVarP(
		&sensorId,
		"id", "i",
		"",
		"Sensor ID as specified in the config",
	)
	_ = Command.MarkPersistentFlagRequired("id")
}

func getSensor(id string) (sensors.Sensor, error) {
	configuration.ReadConfigFile()

	controllers := hwmon.GetChips()

	for _, config := range configuration.CurrentConfig.Sensors {
		if config.ID == id {
			if config.HwMon != nil {
				for _, controller := range controllers {
					matched, err := regexp.MatchString("(?i)"+config.HwMon.Platform, controller.Platform)
					if err != nil {
						return nil, errors.New(fmt.Sprintf("Failed to match platform regex of %s (%s) against controller platform %s", config.ID, config.HwMon.Platform, controller.Platform))
					}
					if matched {
						index := config.HwMon.Index - 1
						if len(controller.Sensors) > index {
							sensor := controller.Sensors[index]
							if len(sensor.Input) <= 0 {
								return nil, errors.New(fmt.Sprintf("Unable to find temp input for sensor %s", id))
							}
							config.HwMon.TempInput = sensor.Input
							break
						}
					}
				}
			}

			sensor, err := sensors.NewSensor(config)
			if err != nil {
				return nil, err
			}

			return sensor, nil
		}
	}

	return nil, errors.New(fmt.Sprintf("No sensor with id found: %s", sensorId))
}
