package fan

import (
	"errors"
	"fmt"
	"github.com/markusressel/fan2go/internal/configuration"
	"github.com/markusressel/fan2go/internal/fans"
	"github.com/markusressel/fan2go/internal/hwmon"
	"github.com/spf13/cobra"
	"regexp"
)

var fanId string

var Command = &cobra.Command{
	Use:              "fan",
	Short:            "Fan related commands",
	Long:             ``,
	TraverseChildren: true,
}

func init() {
	Command.PersistentFlags().StringVarP(
		&fanId,
		"id", "i",
		"",
		"Fan ID as specified in the config",
	)
	_ = Command.MarkPersistentFlagRequired("id")
}

func getFan(id string) (fans.Fan, error) {
	configuration.ReadConfigFile()

	controllers := hwmon.GetChips()

	for _, config := range configuration.CurrentConfig.Fans {
		if config.ID == id {
			if config.HwMon != nil {
				for _, controller := range controllers {
					matched, err := regexp.MatchString("(?i)"+config.HwMon.Platform, controller.Platform)
					if err != nil {
						return nil, errors.New(fmt.Sprintf("Failed to match platform regex of %s (%s) against controller platform %s", config.ID, config.HwMon.Platform, controller.Platform))
					}
					if matched {
						index := config.HwMon.Index - 1
						if len(controller.Fans) > index {
							fan := controller.Fans[index]
							config.HwMon.PwmOutput = fan.PwmOutput
							config.HwMon.RpmInput = fan.RpmInput
							break
						}
					}
				}
			}

			fan, err := fans.NewFan(config)
			if err != nil {
				return nil, err
			}

			return fan, nil
		}
	}

	return nil, errors.New(fmt.Sprintf("No fan with id found: %s", fanId))
}
