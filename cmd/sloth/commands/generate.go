package commands

import (
	"context"
	"fmt"
	"io"
	"os"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/slok/sloth/internal/alert"
	"github.com/slok/sloth/internal/app/generate"
	"github.com/slok/sloth/internal/info"
	"github.com/slok/sloth/internal/k8sprometheus"
	"github.com/slok/sloth/internal/log"
	"github.com/slok/sloth/internal/prometheus"
	kubernetesv1 "github.com/slok/sloth/pkg/kubernetes/api/sloth/v1"
	prometheusv1 "github.com/slok/sloth/pkg/prometheus/api/v1"
)

type generateCommand struct {
	slosInput         string
	slosOut           string
	disableRecordings bool
	disableAlerts     bool
	chronoVersion     bool
	extraLabels       map[string]string
	sliPluginsPaths   []string
}

// NewGenerateCommand returns the generate command.
func NewGenerateCommand(app *kingpin.Application) Command {
	c := &generateCommand{extraLabels: map[string]string{}}
	cmd := app.Command("generate", "Generates Prometheus SLOs.")
	cmd.Flag("input", "SLO spec input file path.").Short('i').Required().StringVar(&c.slosInput)
	cmd.Flag("out", "Generated rules output file path. If `-` it will use stdout.").Short('o').Default("-").StringVar(&c.slosOut)
	cmd.Flag("extra-labels", "Extra labels that will be added to all the generated Prometheus rules ('key=value' form, can be repeated).").Short('l').StringMapVar(&c.extraLabels)
	cmd.Flag("disable-recordings", "Disables recording rules generation.").BoolVar(&c.disableRecordings)
	cmd.Flag("disable-alerts", "Disables alert rules generation.").BoolVar(&c.disableAlerts)
	cmd.Flag("sli-plugins-path", "The path to SLI plugins (can be repeated), if not set it disable plugins support.").Short('p').StringsVar(&c.sliPluginsPaths)
	cmd.Flag("chrono", "Create chronosphere compatible output.").Short('c').BoolVar(&c.chronoVersion)

	return c
}

func (g generateCommand) Name() string { return "generate" }
func (g generateCommand) Run(ctx context.Context, config RootConfig) error {
	ctx = config.Logger.SetValuesOnCtx(ctx, log.Kv{
		"out": g.slosOut,
	})

	// Get SLO spec data.
	// TODO(slok): stdin.
	f, err := os.Open(g.slosInput)
	if err != nil {
		return fmt.Errorf("could not open SLOs spec file: %w", err)
	}
	defer f.Close()

	slxData, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("could not read SLOs spec file data: %w", err)
	}

	// Load plugins
	pluginRepo, err := createPluginLoader(ctx, config.Logger, g.sliPluginsPaths)
	if err != nil {
		return err
	}

	// Create Spec loaders.
	promYAMLLoader := prometheus.NewYAMLSpecLoader(pluginRepo)
	kubeYAMLLoader := k8sprometheus.NewYAMLSpecLoader(pluginRepo)

	// Prepare store output.
	var out io.Writer = config.Stdout
	if g.slosOut != "-" {
		f, err := os.Create(g.slosOut)
		if err != nil {
			return fmt.Errorf("could not create out file: %w", err)
		}
		defer f.Close()
		out = f
	}

	// Split YAMLs in case we have multiple yaml files in a single file.
	splittedSLOsData := splitYAML(slxData)

	for _, data := range splittedSLOsData {
		// Try loading spec with all the generators possible:
		// 1 - Raw Prometheus generator.
		slos, promErr := promYAMLLoader.LoadSpec(ctx, []byte(data))
		if promErr == nil {
			err := generatePrometheus(ctx, config.Logger, g.disableRecordings, g.disableAlerts, g.chronoVersion, g.extraLabels, *slos, out)
			if err != nil {
				return fmt.Errorf("could not generate Prometheus format rules: %w", err)
			}
			continue
		}

		// 2 - Kubernetes Prometheus operator generator.
		sloGroup, k8sErr := kubeYAMLLoader.LoadSpec(ctx, []byte(data))
		if k8sErr == nil {
			err := generateKubernetes(ctx, config.Logger, g.disableRecordings, g.disableAlerts, g.extraLabels, *sloGroup, out)
			if err != nil {
				return fmt.Errorf("could not generate Kubernetes format rules: %w", err)
			}
			continue
		}

		// If we reached here means that we could not use any of the available spec types.
		config.Logger.Errorf("Tried loading raw prometheus SLOs spec, it couldn't: %s", promErr)
		config.Logger.Errorf("Tried loading Kubernetes prometheus SLOs spec, it couldn't: %s", k8sErr)
		return fmt.Errorf("invalid spec, could not load with any of the supported spec types")
	}

	return nil
}

// generatePrometheus generates the SLOs based on a raw regular Prometheus spec format input and
// outs a Prometheus raw yaml.
func generatePrometheus(ctx context.Context, logger log.Logger, disableRecs, disableAlerts, chronoVersion bool, extraLabels map[string]string, slos prometheus.SLOGroup, out io.Writer) error {
	logger.Infof("Generating from Prometheus spec")
	info := info.Info{
		Version: info.Version,
		Mode:    info.ModeCLIGenPrometheus,
		Spec:    prometheusv1.Version,
	}

	result, err := generateRules(ctx, logger, info, disableRecs, disableAlerts, chronoVersion, extraLabels, slos)
	if err != nil {
		return err
	}

	repo := prometheus.NewIOWriterGroupedRulesYAMLRepo(out, logger)
	storageSLOs := make([]prometheus.StorageSLO, 0, len(result.PrometheusSLOs))
	for _, s := range result.PrometheusSLOs {
		storageSLOs = append(storageSLOs, prometheus.StorageSLO{
			SLO:   s.SLO,
			Rules: s.SLORules,
		})
	}

	err = repo.StoreSLOs(ctx, storageSLOs)
	if err != nil {
		return fmt.Errorf("could not store SLOS: %w", err)
	}

	return nil
}

// generateKubernetes generates the SLOs based on a Kuberentes spec format input and
// outs a Kubernetes prometheus operator CRD yaml.
func generateKubernetes(ctx context.Context, logger log.Logger, disableRecs, disableAlerts bool, extraLabels map[string]string, sloGroup k8sprometheus.SLOGroup, out io.Writer) error {
	logger.Infof("Generating from Kubernetes Prometheus spec")

	info := info.Info{
		Version: info.Version,
		Mode:    info.ModeCLIGenKubernetes,
		Spec:    fmt.Sprintf("%s/%s", kubernetesv1.SchemeGroupVersion.Group, kubernetesv1.SchemeGroupVersion.Version),
	}
	result, err := generateRules(ctx, logger, info, disableRecs, disableAlerts, false, extraLabels, sloGroup.SLOGroup)
	if err != nil {
		return err
	}

	repo := k8sprometheus.NewIOWriterPrometheusOperatorYAMLRepo(out, logger)
	storageSLOs := make([]k8sprometheus.StorageSLO, 0, len(result.PrometheusSLOs))
	for _, s := range result.PrometheusSLOs {
		storageSLOs = append(storageSLOs, k8sprometheus.StorageSLO{
			SLO:   s.SLO,
			Rules: s.SLORules,
		})
	}

	err = repo.StoreSLOs(ctx, sloGroup.K8sMeta, storageSLOs)
	if err != nil {
		return fmt.Errorf("could not store SLOS: %w", err)
	}

	return nil
}

// generate is the main generator logic that all the spec types and storers share. Mainly
// has the logic of the generate app service.
func generateRules(ctx context.Context, logger log.Logger, info info.Info, disableRecs, disableAlerts, chronoVersion bool, extraLabels map[string]string, slos prometheus.SLOGroup) (*generate.Response, error) {
	// Disable recording rules if required.
	var sliRuleGen generate.SLIRecordingRulesGenerator = generate.NoopSLIRecordingRulesGenerator
	var metaRuleGen generate.MetadataRecordingRulesGenerator = generate.NoopMetadataRecordingRulesGenerator
	if !disableRecs {
		sliRuleGen = prometheus.SLIRecordingRulesGenerator
		metaRuleGen = prometheus.MetadataRecordingRulesGenerator
	}

	// Disable alert rules if required.
	var alertRuleGen generate.SLOAlertRulesGenerator = generate.NoopSLOAlertRulesGenerator
	if !disableAlerts {
		alertRuleGen = prometheus.SLOAlertRulesGenerator
	}

	if chronoVersion {
		alertRuleGen = prometheus.SLOAlertRulesGeneratorChrono
	}

	// Generate.
	controller, err := generate.NewService(generate.ServiceConfig{
		AlertGenerator:              alert.AlertGenerator,
		SLIRecordingRulesGenerator:  sliRuleGen,
		MetaRecordingRulesGenerator: metaRuleGen,
		SLOAlertRulesGenerator:      alertRuleGen,
		Logger:                      logger,
	})
	if err != nil {
		return nil, fmt.Errorf("could not create application service: %w", err)
	}

	result, err := controller.Generate(ctx, generate.Request{
		ExtraLabels: extraLabels,
		Info:        info,
		SLOGroup:    slos,
	})
	if err != nil {
		return nil, fmt.Errorf("could not generate prometheus rules: %w", err)
	}

	return result, nil
}
