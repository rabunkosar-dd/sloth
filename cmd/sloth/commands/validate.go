package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/slok/sloth/internal/k8sprometheus"
	"github.com/slok/sloth/internal/log"
	"github.com/slok/sloth/internal/prometheus"
	"gopkg.in/alecthomas/kingpin.v2"
)

type validateCommand struct {
	slosInput        string
	slosExcludeRegex string
	slosIncludeRegex string
	extraLabels      map[string]string
	sliPluginsPaths  []string
}

// NewValidateCommand returns the validate command.
func NewValidateCommand(app *kingpin.Application) Command {
	c := &validateCommand{extraLabels: map[string]string{}}
	cmd := app.Command("validate", "Validates the SLO manifests and generation of Prometheus SLOs.")
	cmd.Flag("input", "SLO spec discovery path, will discover recursively all YAML files.").Short('i').Required().StringVar(&c.slosInput)
	cmd.Flag("fs-exclude", "Filter regex to ignore matched discovered SLO file paths.").Short('e').StringVar(&c.slosExcludeRegex)
	cmd.Flag("fs-include", "Filter regex to include matched discovered SLO file paths, everything else will be ignored. Exclude has preference.").Short('n').StringVar(&c.slosIncludeRegex)
	cmd.Flag("extra-labels", "Extra labels that will be added to all the generated Prometheus rules ('key=value' form, can be repeated).").Short('l').StringMapVar(&c.extraLabels)
	cmd.Flag("sli-plugins-path", "The path to SLI plugins (can be repeated), if not set it disable plugins support.").Short('p').StringsVar(&c.sliPluginsPaths)

	return c
}

func (v validateCommand) Name() string { return "validate" }
func (v validateCommand) Run(ctx context.Context, config RootConfig) error {
	// Set up files discovery filter regex.
	var excludeRegex *regexp.Regexp
	var includeRegex *regexp.Regexp
	if v.slosExcludeRegex != "" {
		r, err := regexp.Compile(v.slosExcludeRegex)
		if err != nil {
			return fmt.Errorf("invalid exclude regex: %w", err)
		}
		excludeRegex = r
	}
	if v.slosIncludeRegex != "" {
		r, err := regexp.Compile(v.slosIncludeRegex)
		if err != nil {
			return fmt.Errorf("invalid include regex: %w", err)
		}
		includeRegex = r
	}

	// Discover SLOs.
	sloPaths, err := discoverSLOManifests(config.Logger, excludeRegex, includeRegex, v.slosInput)
	if err != nil {
		return fmt.Errorf("could not discover files: %w", err)
	}
	if len(sloPaths) == 0 {
		return fmt.Errorf("0 slo specs have been discovered")
	}

	// Load plugins.
	pluginRepo, err := createPluginLoader(ctx, config.Logger, v.sliPluginsPaths)
	if err != nil {
		return err
	}

	// Create Spec loaders.
	promYAMLLoader := prometheus.NewYAMLSpecLoader(pluginRepo)
	kubeYAMLLoader := k8sprometheus.NewYAMLSpecLoader(pluginRepo)

	// For every file load the data and start the validation process:
	validations := []*fileValidation{}
	totalValidations := 0
	for _, input := range sloPaths {
		// Get SLO spec data.
		slxData, err := os.ReadFile(input)
		if err != nil {
			return fmt.Errorf("could not read SLOs spec file data: %w", err)
		}

		// Split YAMLs in case we have multiple yaml files in a single file.
		splittedSLOsData := splitYAML(slxData)

		// Prepare file validation result and start validation result for every SLO in the file.
		// TODO(slok): Add service meta to validation.
		validation := &fileValidation{File: input}
		validations = append(validations, validation)
		for _, data := range splittedSLOsData {
			totalValidations++

			// Try loading spec with all the generators possible:
			// 1 - Raw Prometheus generator.
			slos, promErr := promYAMLLoader.LoadSpec(ctx, []byte(data))
			if promErr == nil {
				err := generatePrometheus(ctx, log.Noop, false, false, false, v.extraLabels, *slos, io.Discard)
				if err != nil {
					validation.Errs = []error{fmt.Errorf("could not generate Prometheus format rules: %w", err)}
				}
				continue
			}

			// 2 - Kubernetes Prometheus operator generator.
			sloGroup, k8sErr := kubeYAMLLoader.LoadSpec(ctx, []byte(data))
			if k8sErr == nil {
				err := generateKubernetes(ctx, log.Noop, false, false, v.extraLabels, *sloGroup, io.Discard)
				if err != nil {
					validation.Errs = []error{fmt.Errorf("could not generate Kubernetes format rules: %w", err)}
				}
				continue
			}

			// If we reached here means that we could not use any of the available spec types.
			validation.Errs = []error{
				fmt.Errorf("Tried loading raw prometheus SLOs spec, it couldn't: %w", promErr),
				fmt.Errorf("Tried loading Kubernetes prometheus SLOs spec, it couldn't: %w", k8sErr),
			}
		}

		// Don't wait until the end to show validation per file.
		logger := config.Logger.WithValues(log.Kv{"file": validation.File})
		logger.Debugf("File validated")
		for _, err := range validation.Errs {
			logger.Errorf("%s", err)
		}
	}

	// Check if we need to return an error.
	for _, v := range validations {
		if len(v.Errs) != 0 {
			return fmt.Errorf("validation failed")
		}
	}

	config.Logger.WithValues(log.Kv{"slo-specs": totalValidations}).Infof("Validation succeeded")
	return nil
}

type fileValidation struct {
	File string
	Errs []error
}
