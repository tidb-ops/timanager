package command

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	"github.com/tidbops/tim/pkg/models"
	"github.com/tidbops/tim/pkg/parser"
	"github.com/tidbops/tim/pkg/utils"
	tyaml "github.com/tidbops/tim/pkg/yaml"
	yaml "gopkg.in/mikefarah/yaml.v2"
)

const (
	tikvRawConfigURL = "https://raw.githubusercontent.com/pingcap/tidb-ansible/%s/conf/tikv.yml"
)

const (
	InputNew     = "Input a new config file"
	UseOrigin    = "Use the origin config file"
	UseRuleFiles = "Use the configuration rules file to generate a new configuration file?"
)

type UpgradeCommandFlags struct {
	TargetVersion string
	RuleFile      string
}

var (
	upgradeCmdFlags = &UpgradeCommandFlags{}
)

func NewUpgradeCommand() *cobra.Command {
	upgradeCmd := &cobra.Command{
		Use:   "upgrade <name>",
		Short: "upgrade tidb version, just generate the new version tidb-ansible files",
		Run:   upgradeCommandFunc,
	}

	upgradeCmd.Flags().StringVar(&upgradeCmdFlags.TargetVersion, "target-version", "", "the version that ready to upgrade to")
	upgradeCmd.Flags().StringVar(&upgradeCmdFlags.RuleFile, "rule-file", "",
		"rule files for different version of configuration conversion")

	return upgradeCmd
}

func upgradeCommandFunc(cmd *cobra.Command, args []string) {
	if len(args) < 0 {
		cmd.Println("name is required")
		cmd.Println(cmd.UsageString())
		return
	}

	if upgradeCmdFlags.TargetVersion == "" {
		cmd.Println("target-version flag is required")
		cmd.Println(cmd.UsageString())
		return
	}

	name := args[0]
	cli, err := genClient(cmd)
	if err != nil {
		cmd.Printf("init client failed, %v\n", err)
	}

	tc, err := cli.GetTiDBClusterByName(name)
	if err != nil {
		cmd.Printf("%s tidb cluster not exist\n", name)
		return
	}

	tmpID := time.Now().Unix()
	tmpPath := fmt.Sprintf("/tmp/tim/%s/%d", tc.Name, tmpID)

	// just prepare tikv config fot demo
	// TODO: support prepare pd / tidb config
	oldTiKVConfig, targetTiKVConfig, err := prepareConfigFile(tc, upgradeCmdFlags.TargetVersion, tmpPath)
	if err != nil {
		cmd.Println("prepare config file failed, %v", err)
		return
	}

	diffStr, err := tyaml.Diff(oldTiKVConfig, targetTiKVConfig, true)
	if err != nil {
		cmd.Printf("compare %s %s failed, %v\n", oldTiKVConfig, targetTiKVConfig, err)
		return
	}

	if len(diffStr) > 0 {
		cmd.Println("Default tikv config has changed!")
		cmd.Println(diffStr)
	}

	prompt := promptui.Select{
		Label: "Select to init Config",
		Items: []string{
			InputNew,
			UseOrigin,
			UseRuleFiles,
		},
	}

	_, result, err := prompt.Run()
	if err != nil {
		cmd.Println(err)
		return
	}

	srcTiKVConfigFile := fmt.Sprintf("%s/conf/tikv.yml", tc.Path)
	distTiKVConfigFile := fmt.Sprintf("%s/tikv-origin.yml", tmpPath)
	if err := utils.CopyFile(srcTiKVConfigFile, distTiKVConfigFile); err != nil {
		cmd.Println(err)
		return
	}

	var targetTiKVConfigFile string

	switch result {
	case InputNew:
	case UseOrigin:
	case UseRuleFiles:
		_, targetTiKVConfigFile, err = generateConfigByRuleFile(cmd, distTiKVConfigFile, tmpPath, "tikv")
	default:
		cmd.Printf("%s is invalid\n", result)
		return
	}

	bakDir := fmt.Sprintf("%s-%s-bak", tc.Path, tc.Version)
	if err := os.Rename(tc.Path, bakDir); err != nil {
		cmd.Println(err)
		return
	}

	if err := initTiDBAnsible(upgradeCmdFlags.TargetVersion, tc.Path); err != nil {
		cmd.Println(err)
		return
	}

	if err := copyConfigs(bakDir, tc.Path); err != nil {
		cmd.Println(err)
		return
	}

	if err := utils.CopyFile(targetTiKVConfigFile,
		fmt.Sprintf("%s/conf/tikv.yml", tc.Path)); err != nil {
		cmd.Println(err)
		return
	}
	tc.Version = upgradeCmdFlags.TargetVersion
	tc.Status = models.TiDBWaitingUpgrade
	if err := cli.UpdateTiDBCluster(tc); err != nil {
		fmt.Println(err)
		return
	}

	cmd.Println("Success! Init %s tidb-ansible files saved to %s", upgradeCmdFlags.TargetVersion, tc.Path)
	cmd.Println("You can execute the following commands to upgrade!!")
	cmd.Printf("cd %s\n", tc.Path)
	cmd.Println("ansible-playbook local_prepare.yml")
	cmd.Println("ansible-playbook excessive_rolling_update.yml")
}

func copyConfigs(src, dist string) error {
	srcInv := fmt.Sprintf("%s/inventory.ini", src)
	distInv := fmt.Sprintf("%s/inventory.ini", dist)

	if err := utils.CopyFile(srcInv, distInv); err != nil {
		return err
	}

	srcConf := fmt.Sprintf("%s/conf", src)
	distConf := fmt.Sprintf("%s/conf", dist)
	if err := utils.CopyDir(srcConf, distConf); err != nil {
		return err
	}

	return nil
}

func generateConfigByRuleFile(
	cmd *cobra.Command,
	configFile string,
	path string,
	prefix string,
) (string, string, error) {
	validate := func(input string) error {
		if exist := utils.FileExists(input); !exist {
			return fmt.Errorf("file %s not exist", input)
		}

		return nil
	}

	prompt := promptui.Prompt{
		Label:    "Rule File",
		Validate: validate,
	}

	result, err := prompt.Run()
	if err != nil {
		return "", "", err
	}

	p := parser.NewParser()
	newRuleFile, deleteRuleFile, err := p.ParserFile(result, path, prefix)
	if err != nil {
		return "", "", err
	}

	deleteRules := &DeleteRules{}

	deleteRuleData, err := ioutil.ReadFile(deleteRuleFile)
	if err != nil {
		return "", "", err
	}

	if err := yaml.Unmarshal(deleteRuleData, deleteRules); err != nil {
		return "", "", err
	}

	output, err := tyaml.DeleteMulti(configFile, deleteRules.Delete)
	if err != nil {
		return "", "", err
	}

	waitingForMergeFile := fmt.Sprintf("%s-%s-waiting-merge.yml", path, prefix)

	if err := utils.WriteToFile(output, waitingForMergeFile); err != nil {
		return "", "", err
	}

	output, err = tyaml.Merge(true, false, waitingForMergeFile, newRuleFile)
	if err != nil {
		return "", "", err
	}

	targetConfigFile := fmt.Sprintf("%s/%s-target-config.yml", path, prefix)
	if err := utils.WriteToFile(output, targetConfigFile); err != nil {
		return "", "", err
	}

	return output, targetConfigFile, nil
}

type DeleteRules struct {
	Delete []string `yaml:"delete"`
}

func prepareConfigFile(tc *models.TiDBCluster, targetVersion string, path string) (string, string, error) {
	if err := os.MkdirAll(path, os.ModePerm); err != nil {
		return "", "", err
	}

	oldRawTiKVConfigURL := fmt.Sprintf(tikvRawConfigURL, tc.Version)
	oldTiKVConfigPath := filepath.Join(path, fmt.Sprintf("%s-tikv.yml", tc.Version))
	if err := DownloadFile(oldRawTiKVConfigURL, oldTiKVConfigPath); err != nil {
		return "", "", err
	}

	targetRawTiKVConfigURL := fmt.Sprintf(tikvRawConfigURL, targetVersion)
	targetTiKVConfigPath := filepath.Join(path, fmt.Sprintf("%s-tikv.yml", targetVersion))
	if err := DownloadFile(targetRawTiKVConfigURL, targetTiKVConfigPath); err != nil {
		return "", "", err
	}

	return oldTiKVConfigPath, targetTiKVConfigPath, nil
}
