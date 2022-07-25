package cloud

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencost/opencost/pkg/clustercache"
	"github.com/opencost/opencost/pkg/env"
	"github.com/opencost/opencost/pkg/log"
	"github.com/opencost/opencost/pkg/util"
	"github.com/opencost/opencost/pkg/util/fileutil"
	"github.com/opencost/opencost/pkg/util/json"

	"github.com/Azure/azure-sdk-for-go/services/preview/commerce/mgmt/2015-06-01-preview/commerce"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2016-06-01/subscriptions"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2018-05-01/resources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	v1 "k8s.io/api/core/v1"
)

const (
	AzureFilePremiumStorageClass     = "premium_smb"
	AzureFileStandardStorageClass    = "standard_smb"
	AzureDiskPremiumSSDStorageClass  = "premium_ssd"
	AzureDiskStandardSSDStorageClass = "standard_ssd"
	AzureDiskStandardStorageClass    = "standard_hdd"
	defaultSpotLabel                 = "kubernetes.azure.com/scalesetpriority"
	defaultSpotLabelValue            = "spot"
	AzureStorageUpdateType           = "AzureStorage"
)

var (
	regionCodeMappings = map[string]string{
		"ap": "asia",
		"au": "australia",
		"br": "brazil",
		"ca": "canada",
		"eu": "europe",
		"fr": "france",
		"in": "india",
		"ja": "japan",
		"kr": "korea",
		"uk": "uk",
		"us": "us",
		"za": "southafrica",
		"no": "norway",
	}

	//mtBasic, _     = regexp.Compile("^BASIC.A\\d+[_Promo]*$")
	//mtStandardA, _ = regexp.Compile("^A\\d+[_Promo]*$")
	mtStandardB, _ = regexp.Compile(`^Standard_B\d+m?[_v\d]*[_Promo]*$`)
	mtStandardD, _ = regexp.Compile(`^Standard_D\d[_v\d]*[_Promo]*$`)
	mtStandardE, _ = regexp.Compile(`^Standard_E\d+i?[_v\d]*[_Promo]*$`)
	mtStandardF, _ = regexp.Compile(`^Standard_F\d+[_v\d]*[_Promo]*$`)
	mtStandardG, _ = regexp.Compile(`^Standard_G\d+[_v\d]*[_Promo]*$`)
	mtStandardL, _ = regexp.Compile(`^Standard_L\d+[_v\d]*[_Promo]*$`)
	mtStandardM, _ = regexp.Compile(`^Standard_M\d+[m|t|l]*s[_v\d]*[_Promo]*$`)
	mtStandardN, _ = regexp.Compile(`^Standard_N[C|D|V]\d+r?[_v\d]*[_Promo]*$`)

	// azure:///subscriptions/0badafdf-1234-abcd-wxyz-123456789/...
	//  => 0badafdf-1234-abcd-wxyz-123456789
	azureSubRegex = regexp.MustCompile("azure:///subscriptions/([^/]*)/*")
)

// List obtained by installing the Azure CLI tool "az", described here:
// https://docs.microsoft.com/en-us/cli/azure/install-azure-cli-linux?pivots=apt
// logging into an Azure account, and running command `az account list-locations`
var azureRegions = []string{
	"eastus",
	"eastus2",
	"southcentralus",
	"westus2",
	"westus3",
	"australiaeast",
	"southeastasia",
	"northeurope",
	"swedencentral",
	"uksouth",
	"westeurope",
	"centralus",
	"northcentralus",
	"westus",
	"southafricanorth",
	"centralindia",
	"eastasia",
	"japaneast",
	"jioindiawest",
	"koreacentral",
	"canadacentral",
	"francecentral",
	"germanywestcentral",
	"norwayeast",
	"switzerlandnorth",
	"uaenorth",
	"brazilsouth",
	"centralusstage",
	"eastusstage",
	"eastus2stage",
	"northcentralusstage",
	"southcentralusstage",
	"westusstage",
	"westus2stage",
	"asia",
	"asiapacific",
	"australia",
	"brazil",
	"canada",
	"europe",
	"france",
	"germany",
	"global",
	"india",
	"japan",
	"korea",
	"norway",
	"southafrica",
	"switzerland",
	"uae",
	"uk",
	"unitedstates",
	"eastasiastage",
	"southeastasiastage",
	"centraluseuap",
	"eastus2euap",
	"westcentralus",
	"southafricawest",
	"australiacentral",
	"australiacentral2",
	"australiasoutheast",
	"japanwest",
	"jioindiacentral",
	"koreasouth",
	"southindia",
	"westindia",
	"canadaeast",
	"francesouth",
	"germanynorth",
	"norwaywest",
	"switzerlandwest",
	"ukwest",
	"uaecentral",
	"brazilsoutheast",
	"usgovarizona",
	"usgoviowa",
	"usgovvirginia",
	"usgovtexas",
}

type regionParts []string

func (r regionParts) String() string {
	var result string
	for _, p := range r {
		result += p
	}
	return result
}

func getRegions(service string, subscriptionsClient subscriptions.Client, providersClient resources.ProvidersClient, subscriptionID string) (map[string]string, error) {

	allLocations := make(map[string]string)
	supLocations := make(map[string]string)

	// retrieve all locations for the subscription id (some of them may not be supported by the required provider)
	if locations, err := subscriptionsClient.ListLocations(context.TODO(), subscriptionID); err == nil {
		// fill up the map: DisplayName - > Name
		for _, loc := range *locations.Value {
			allLocations[*loc.DisplayName] = *loc.Name
		}
	} else {
		return nil, err
	}

	// identify supported locations for the namespace and resource type
	const (
		providerNamespaceForCompute = "Microsoft.Compute"
		resourceTypeForCompute      = "locations/vmSizes"
		providerNamespaceForAks     = "Microsoft.ContainerService"
		resourceTypeForAks          = "managedClusters"
	)

	switch service {
	case "aks":
		if providers, err := providersClient.Get(context.TODO(), providerNamespaceForAks, ""); err == nil {
			for _, pr := range *providers.ResourceTypes {
				if *pr.ResourceType == resourceTypeForAks {
					for _, displName := range *pr.Locations {
						if loc, ok := allLocations[displName]; ok {
							supLocations[loc] = displName
						} else {
							log.Warnf("unsupported cloud region %s", loc)
						}
					}
					break
				}
			}
		} else {
			return nil, err
		}
		return supLocations, nil
	default:
		if providers, err := providersClient.Get(context.TODO(), providerNamespaceForCompute, ""); err == nil {
			for _, pr := range *providers.ResourceTypes {
				if *pr.ResourceType == resourceTypeForCompute {
					for _, displName := range *pr.Locations {
						if loc, ok := allLocations[displName]; ok {
							supLocations[loc] = displName
						} else {
							log.Warnf("unsupported cloud region %s", loc)
						}
					}
					break
				}
			}
		} else {
			return nil, err
		}

		return supLocations, nil
	}
}

func getRetailPrice(region string, skuName string, currencyCode string, spot bool) (string, error) {
	pricingURL := "https://prices.azure.com/api/retail/prices?$skip=0"

	if currencyCode != "" {
		pricingURL += fmt.Sprintf("&currencyCode='%s'", currencyCode)
	}

	var filterParams []string

	if region != "" {
		regionParam := fmt.Sprintf("armRegionName eq '%s'", region)
		filterParams = append(filterParams, regionParam)
	}

	if skuName != "" {
		skuNameParam := fmt.Sprintf("armSkuName eq '%s'", skuName)
		filterParams = append(filterParams, skuNameParam)
	}

	if len(filterParams) > 0 {
		filterParamsEscaped := url.QueryEscape(strings.Join(filterParams[:], " and "))
		pricingURL += fmt.Sprintf("&$filter=%s", filterParamsEscaped)
	}

	log.Infof("starting download retail price payload from \"%s\"", pricingURL)
	resp, err := http.Get(pricingURL)

	if err != nil {
		return "", fmt.Errorf("bogus fetch of \"%s\": %v", pricingURL, err)
	}

	if resp.StatusCode < 200 && resp.StatusCode > 299 {
		return "", fmt.Errorf("retail price responded with error status code %d", resp.StatusCode)
	}

	pricingPayload := AzureRetailPricing{}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("Error getting response: %v", err)
	}

	jsonErr := json.Unmarshal(body, &pricingPayload)
	if jsonErr != nil {
		return "", fmt.Errorf("Error unmarshalling data: %v", jsonErr)
	}

	retailPrice := ""
	for _, item := range pricingPayload.Items {
		if item.Type == "Consumption" && !strings.Contains(item.ProductName, "Windows") {
			// if spot is true SkuName should contain "spot, if it is false it should not
			if spot == strings.Contains(strings.ToLower(item.SkuName), " spot") {
				retailPrice = fmt.Sprintf("%f", item.RetailPrice)
			}
		}
	}

	log.DedupedInfof(5, "done parsing retail price payload from \"%s\"\n", pricingURL)

	if retailPrice == "" {
		return retailPrice, fmt.Errorf("Couldn't find price for product \"%s\" in \"%s\" region", skuName, region)
	}

	return retailPrice, nil
}

func toRegionID(meterRegion string, regions map[string]string) (string, error) {
	var rp regionParts = strings.Split(strings.ToLower(meterRegion), " ")
	regionCode := regionCodeMappings[rp[0]]
	lastPart := rp[len(rp)-1]
	var regionIds []string
	if regionID, ok := regionIdByDisplayName[meterRegion]; ok {
		regionIds = []string{
			regionID,
		}
	} else if _, err := strconv.Atoi(lastPart); err == nil {
		regionIds = []string{
			fmt.Sprintf("%s%s%s", regionCode, rp[1:len(rp)-1], lastPart),
			fmt.Sprintf("%s%s%s", rp[1:len(rp)-1], regionCode, lastPart),
		}
	} else {
		regionIds = []string{
			fmt.Sprintf("%s%s", regionCode, rp[1:]),
			fmt.Sprintf("%s%s", rp[1:], regionCode),
		}
	}
	for _, regionID := range regionIds {
		if checkRegionID(regionID, regions) {
			return regionID, nil
		}
	}
	return "", fmt.Errorf("Couldn't find region")
}

// azure has very inconsistent naming standards between display names from the rate card api and display names from the regions api
// this map is to connect display names from the ratecard api to the appropriate id.
var regionIdByDisplayName = map[string]string{
	"US Gov AZ": "usgovarizona",
	"US Gov TX": "usgovtexas",
	"US Gov":    "usgovvirginia",
}

func checkRegionID(regionID string, regions map[string]string) bool {
	for region := range regions {
		if regionID == region {
			return true
		}
	}
	return false
}

// AzureRetailPricing struct for unmarshalling Azure Retail pricing api JSON response
type AzureRetailPricing struct {
	BillingCurrency    string                         `json:"BillingCurrency"`
	CustomerEntityId   string                         `json:"CustomerEntityId"`
	CustomerEntityType string                         `json:"CustomerEntityType"`
	Items              []AzureRetailPricingAttributes `json:"Items"`
	NextPageLink       string                         `json:"NextPageLink"`
	Count              int                            `json:"Count"`
}

//AzureRetailPricingAttributes struct for unmarshalling Azure Retail pricing api JSON response
type AzureRetailPricingAttributes struct {
	CurrencyCode         string     `json:"currencyCode"`
	TierMinimumUnits     float32    `json:"tierMinimumUnits"`
	RetailPrice          float32    `json:"retailPrice"`
	UnitPrice            float32    `json:"unitPrice"`
	ArmRegionName        string     `json:"armRegionName"`
	Location             string     `json:"location"`
	EffectiveStartDate   *time.Time `json:"effectiveStartDate"`
	EffectiveEndDate     *time.Time `json:"effectiveEndDate"`
	MeterId              string     `json:"meterId"`
	MeterName            string     `json:"meterName"`
	ProductId            string     `json:"productId"`
	SkuId                string     `json:"skuId"`
	ProductName          string     `json:"productName"`
	SkuName              string     `json:"skuName"`
	ServiceName          string     `json:"serviceName"`
	ServiceId            string     `json:"serviceId"`
	ServiceFamily        string     `json:"serviceFamily"`
	UnitOfMeasure        string     `json:"unitOfMeasure"`
	Type                 string     `json:"type"`
	IsPrimaryMeterRegion bool       `json:"isPrimaryMeterRegion"`
	ArmSkuName           string     `json:"armSkuName"`
}

// AzurePricing either contains a Node or PV
type AzurePricing struct {
	Node *Node
	PV   *PV
}

type Azure struct {
	Pricing                        map[string]*AzurePricing
	DownloadPricingDataLock        sync.RWMutex
	Clientset                      clustercache.ClusterCache
	Config                         *ProviderConfig
	serviceAccountChecks           *ServiceAccountChecks
	RateCardPricingError           error
	clusterAccountId               string
	clusterRegion                  string
	loadedAzureSecret              bool
	azureSecret                    *AzureServiceKey
	loadedAzureStorageConfigSecret bool
	azureStorageConfig             *AzureStorageConfig
}

type azureKey struct {
	Labels        map[string]string
	GPULabel      string
	GPULabelValue string
}

func (k *azureKey) Features() string {
	r, _ := util.GetRegion(k.Labels)
	region := strings.ToLower(r)
	instance, _ := util.GetInstanceType(k.Labels)
	usageType := "ondemand"
	return fmt.Sprintf("%s,%s,%s", region, instance, usageType)
}

// GPUType returns value of GPULabel if present
func (k *azureKey) GPUType() string {
	if t, ok := k.Labels[k.GPULabel]; ok {
		return t
	}
	return ""
}

func (k *azureKey) isValidGPUNode() bool {
	return k.GPUType() == k.GPULabelValue && k.GetGPUCount() != "0"
}

func (k *azureKey) ID() string {
	return ""
}

func (k *azureKey) GetGPUCount() string {
	instance, _ := util.GetInstanceType(k.Labels)
	// Double digits that could get matches lower in logic
	if strings.Contains(instance, "NC64") {
		return "4"
	}
	if strings.Contains(instance, "ND96") ||
		strings.Contains(instance, "ND40") {
		return "8"
	}

	// Ordered asc because of some series have different gpu counts on different versions
	if strings.Contains(instance, "NC6") ||
		strings.Contains(instance, "NC4") ||
		strings.Contains(instance, "NC8") ||
		strings.Contains(instance, "NC16") ||
		strings.Contains(instance, "ND6") ||
		strings.Contains(instance, "NV12s") ||
		strings.Contains(instance, "NV6") {
		return "1"
	}

	if strings.Contains(instance, "NC12") ||
		strings.Contains(instance, "ND12") ||
		strings.Contains(instance, "NV24s") ||
		strings.Contains(instance, "NV12") {
		return "2"
	}
	if strings.Contains(instance, "NC24") ||
		strings.Contains(instance, "ND24") ||
		strings.Contains(instance, "NV48s") ||
		strings.Contains(instance, "NV24") {
		return "4"
	}
	return "0"
}

// AzureStorageConfig Represents an azure storage config
type AzureStorageConfig struct {
	SubscriptionId string `json:"azureSubscriptionID"`
	AccountName    string `json:"azureStorageAccount"`
	AccessKey      string `json:"azureStorageAccessKey"`
	ContainerName  string `json:"azureStorageContainer"`
	ContainerPath  string `json:"azureContainerPath"`
	AzureCloud     string `json:"azureCloud"`
}

// IsEmpty returns true if all fields in config are empty, false if not.
func (asc *AzureStorageConfig) IsEmpty() bool {
	return asc.SubscriptionId == "" &&
		asc.AccountName == "" &&
		asc.AccessKey == "" &&
		asc.ContainerName == "" &&
		asc.ContainerPath == "" &&
		asc.AzureCloud == ""
}

// Represents an azure app key
type AzureAppKey struct {
	AppID       string `json:"appId"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
	Password    string `json:"password"`
	Tenant      string `json:"tenant"`
}

// Azure service key for a specific subscription
type AzureServiceKey struct {
	SubscriptionID string       `json:"subscriptionId"`
	ServiceKey     *AzureAppKey `json:"serviceKey"`
}

// Validity check on service key
func (ask *AzureServiceKey) IsValid() bool {
	return ask.SubscriptionID != "" &&
		ask.ServiceKey != nil &&
		ask.ServiceKey.AppID != "" &&
		ask.ServiceKey.Password != "" &&
		ask.ServiceKey.Tenant != ""
}

// Loads the azure authentication via configuration or a secret set at install time.
func (az *Azure) getAzureRateCardAuth(forceReload bool, cp *CustomPricing) (subscriptionID, clientID, clientSecret, tenantID string) {
	// 1. Check config values first (set from frontend UI)
	if az.loadedAzureSecret && cp.AzureSubscriptionID != "" && cp.AzureClientID != "" && cp.AzureClientSecret != "" && cp.AzureTenantID != "" {
		subscriptionID = cp.AzureSubscriptionID
		clientID = cp.AzureClientID
		clientSecret = cp.AzureClientSecret
		tenantID = cp.AzureTenantID
		return
	}

	// 2. Check for secret
	s, _ := az.loadAzureAuthSecret(forceReload)
	if s != nil && s.IsValid() {
		subscriptionID = s.SubscriptionID
		clientID = s.ServiceKey.AppID
		clientSecret = s.ServiceKey.Password
		tenantID = s.ServiceKey.Tenant
		return
	}

	// 3. Empty values
	return "", "", "", ""
}

// GetAzureStorageConfig retrieves storage config from secret and sets default values
func (az *Azure) GetAzureStorageConfig(forceReload bool, cp *CustomPricing) (*AzureStorageConfig, error) {
	// default subscription id
	defaultSubscriptionID := cp.AzureSubscriptionID

	// 1. Check Config for storage set up
	asc := &AzureStorageConfig{
		SubscriptionId: cp.AzureStorageSubscriptionID,
		AccountName:    cp.AzureStorageAccount,
		AccessKey:      cp.AzureStorageAccessKey,
		ContainerName:  cp.AzureStorageContainer,
		ContainerPath:  cp.AzureContainerPath,
		AzureCloud:     cp.AzureCloud,
	}

	// check for required fields
	if asc != nil && asc.AccessKey != "" && asc.AccountName != "" && asc.ContainerName != "" && asc.SubscriptionId != "" {
		az.serviceAccountChecks.set("hasStorage", &ServiceAccountCheck{
			Message: "Azure Storage Config exists",
			Status:  true,
		})
		return asc, nil
	}

	// 2. Check for secret
	asc, err := az.loadAzureStorageConfig(forceReload)
	if err != nil {
		log.Errorf("Error, %s", err.Error())
	}
	// To support already configured users, subscriptionID may not be set in secret in which case, the subscriptionID
	// for the rate card API is used
	if asc.SubscriptionId == "" {
		asc.SubscriptionId = defaultSubscriptionID
	}
	// check for required fields
	if asc != nil && asc.AccessKey != "" && asc.AccountName != "" && asc.ContainerName != "" && asc.SubscriptionId == "" {
		az.serviceAccountChecks.set("hasStorage", &ServiceAccountCheck{
			Message: "Azure Storage Config exists",
			Status:  true,
		})
		return asc, nil
	}

	az.serviceAccountChecks.set("hasStorage", &ServiceAccountCheck{
		Message: "Azure Storage Config exists",
		Status:  false,
	})
	return nil, fmt.Errorf("azure storage config not found")

}

// Load once and cache the result (even on failure). This is an install time secret, so
// we don't expect the secret to change. If it does, however, we can force reload using
// the input parameter.
func (az *Azure) loadAzureAuthSecret(force bool) (*AzureServiceKey, error) {
	if !force && az.loadedAzureSecret {
		return az.azureSecret, nil
	}
	az.loadedAzureSecret = true

	exists, err := fileutil.FileExists(authSecretPath)
	if !exists || err != nil {
		return nil, fmt.Errorf("Failed to locate service account file: %s", authSecretPath)
	}

	result, err := ioutil.ReadFile(authSecretPath)
	if err != nil {
		return nil, err
	}

	var ask AzureServiceKey
	err = json.Unmarshal(result, &ask)
	if err != nil {
		return nil, err
	}

	az.azureSecret = &ask
	return &ask, nil
}

// Load once and cache the result (even on failure). This is an install time secret, so
// we don't expect the secret to change. If it does, however, we can force reload using
// the input parameter.
func (az *Azure) loadAzureStorageConfig(force bool) (*AzureStorageConfig, error) {
	if !force && az.loadedAzureStorageConfigSecret {
		return az.azureStorageConfig, nil
	}
	az.loadedAzureStorageConfigSecret = true

	exists, err := fileutil.FileExists(storageConfigSecretPath)
	if !exists || err != nil {
		return nil, fmt.Errorf("Failed to locate azure storage config file: %s", storageConfigSecretPath)
	}

	result, err := ioutil.ReadFile(storageConfigSecretPath)
	if err != nil {
		return nil, err
	}

	var asc AzureStorageConfig
	err = json.Unmarshal(result, &asc)
	if err != nil {
		return nil, err
	}

	az.azureStorageConfig = &asc
	return &asc, nil
}

func (az *Azure) GetKey(labels map[string]string, n *v1.Node) Key {
	cfg, err := az.GetConfig()
	if err != nil {
		log.Infof("Error loading azure custom pricing information")
	}
	// azure defaults, see https://docs.microsoft.com/en-us/azure/aks/gpu-cluster
	gpuLabel := "accelerator"
	gpuLabelValue := "nvidia"
	if cfg.GpuLabel != "" {
		gpuLabel = cfg.GpuLabel
	}
	if cfg.GpuLabelValue != "" {
		gpuLabelValue = cfg.GpuLabelValue
	}
	return &azureKey{
		Labels:        labels,
		GPULabel:      gpuLabel,
		GPULabelValue: gpuLabelValue,
	}
}

// CreateString builds strings effectively
func createString(keys ...string) string {
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
	}
	return b.String()
}

func transformMachineType(subCategory string, mt []string) []string {
	switch {
	case strings.Contains(subCategory, "Basic"):
		return []string{createString("Basic_", mt[0])}
	case len(mt) == 2:
		return []string{createString("Standard_", mt[0]), createString("Standard_", mt[1])}
	default:
		return []string{createString("Standard_", mt[0])}
	}
}

func addSuffix(mt string, suffixes ...string) []string {
	result := make([]string, len(suffixes))
	var suffix string
	parts := strings.Split(mt, "_")
	if len(parts) > 2 {
		for _, p := range parts[2:] {
			suffix = createString(suffix, "_", p)
		}
	}
	for i, s := range suffixes {
		result[i] = createString(parts[0], "_", parts[1], s, suffix)
	}
	return result
}

func getMachineTypeVariants(mt string) []string {
	switch {
	case mtStandardB.MatchString(mt):
		return []string{createString(mt, "s")}
	case mtStandardD.MatchString(mt):
		var result []string
		result = append(result, addSuffix(mt, "s")[0])
		dsType := strings.Replace(mt, "Standard_D", "Standard_DS", -1)
		result = append(result, dsType)
		result = append(result, addSuffix(dsType, "-1", "-2", "-4", "-8")...)
		return result
	case mtStandardE.MatchString(mt):
		return addSuffix(mt, "s", "-2s", "-4s", "-8s", "-16s", "-32s")
	case mtStandardF.MatchString(mt):
		return addSuffix(mt, "s")
	case mtStandardG.MatchString(mt):
		var result []string
		gsType := strings.Replace(mt, "Standard_G", "Standard_GS", -1)
		result = append(result, gsType)
		return append(result, addSuffix(gsType, "-4", "-8", "-16")...)
	case mtStandardL.MatchString(mt):
		return addSuffix(mt, "s")
	case mtStandardM.MatchString(mt) && strings.HasSuffix(mt, "ms"):
		base := strings.TrimSuffix(mt, "ms")
		return addSuffix(base, "-2ms", "-4ms", "-8ms", "-16ms", "-32ms", "-64ms")
	case mtStandardM.MatchString(mt) && (strings.HasSuffix(mt, "ls") || strings.HasSuffix(mt, "ts")):
		return []string{}
	case mtStandardM.MatchString(mt) && strings.HasSuffix(mt, "s"):
		base := strings.TrimSuffix(mt, "s")
		return addSuffix(base, "", "m")
	case mtStandardN.MatchString(mt):
		return addSuffix(mt, "s")
	}
	return []string{}
}

func (az *Azure) GetManagementPlatform() (string, error) {
	nodes := az.Clientset.GetAllNodes()

	if len(nodes) > 0 {
		n := nodes[0]
		providerID := n.Spec.ProviderID
		if strings.Contains(providerID, "aks") {
			return "aks", nil
		}
	}
	return "", nil
}

// DownloadPricingData uses provided azure "best guesses" for pricing
func (az *Azure) DownloadPricingData() error {
	az.DownloadPricingDataLock.Lock()
	defer az.DownloadPricingDataLock.Unlock()

	config, err := az.GetConfig()
	if err != nil {
		az.RateCardPricingError = err
		return err
	}

	// Load the service provider keys
	subscriptionID, clientID, clientSecret, tenantID := az.getAzureRateCardAuth(true, config)
	config.AzureSubscriptionID = subscriptionID
	config.AzureClientID = clientID
	config.AzureClientSecret = clientSecret
	config.AzureTenantID = tenantID

	var authorizer autorest.Authorizer

	azureEnv := determineCloudByRegion(az.clusterRegion)

	if config.AzureClientID != "" && config.AzureClientSecret != "" && config.AzureTenantID != "" {
		credentialsConfig := NewClientCredentialsConfig(config.AzureClientID, config.AzureClientSecret, config.AzureTenantID, azureEnv)
		a, err := credentialsConfig.Authorizer()
		if err != nil {
			az.RateCardPricingError = err
			return err
		}
		authorizer = a
	}

	if authorizer == nil {
		a, err := auth.NewAuthorizerFromEnvironment()
		authorizer = a
		if err != nil {
			a, err := auth.NewAuthorizerFromFile(azureEnv.ResourceManagerEndpoint)
			if err != nil {
				az.RateCardPricingError = err
				return err
			}
			authorizer = a
		}
	}

	sClient := subscriptions.NewClientWithBaseURI(azureEnv.ResourceManagerEndpoint)
	sClient.Authorizer = authorizer

	rcClient := commerce.NewRateCardClientWithBaseURI(azureEnv.ResourceManagerEndpoint, config.AzureSubscriptionID)
	rcClient.Authorizer = authorizer

	providersClient := resources.NewProvidersClientWithBaseURI(azureEnv.ResourceManagerEndpoint, config.AzureSubscriptionID)
	providersClient.Authorizer = authorizer

	rateCardFilter := fmt.Sprintf("OfferDurableId eq '%s' and Currency eq '%s' and Locale eq 'en-US' and RegionInfo eq '%s'", config.AzureOfferDurableID, config.CurrencyCode, config.AzureBillingRegion)

	log.Infof("Using ratecard query %s", rateCardFilter)
	result, err := rcClient.Get(context.TODO(), rateCardFilter)
	if err != nil {
		log.Warnf("Error in pricing download query from API")
		az.RateCardPricingError = err
		return err
	}

	regions, err := getRegions("compute", sClient, providersClient, config.AzureSubscriptionID)
	if err != nil {
		log.Warnf("Error in pricing download regions from API")
		az.RateCardPricingError = err
		return err
	}

	baseCPUPrice := config.CPU
	allPrices := make(map[string]*AzurePricing)

	for _, v := range *result.Meters {
		meterName := *v.MeterName
		meterRegion := *v.MeterRegion
		meterCategory := *v.MeterCategory
		meterSubCategory := *v.MeterSubCategory

		region, err := toRegionID(meterRegion, regions)
		if err != nil {
			continue
		}

		if !strings.Contains(meterSubCategory, "Windows") {

			if strings.Contains(meterCategory, "Storage") {
				if strings.Contains(meterSubCategory, "HDD") || strings.Contains(meterSubCategory, "SSD") || strings.Contains(meterSubCategory, "Premium Files") {
					var storageClass string = ""
					if strings.Contains(meterName, "P4 ") {
						storageClass = AzureDiskPremiumSSDStorageClass
					} else if strings.Contains(meterName, "E4 ") {
						storageClass = AzureDiskStandardSSDStorageClass
					} else if strings.Contains(meterName, "S4 ") {
						storageClass = AzureDiskStandardStorageClass
					} else if strings.Contains(meterName, "LRS Provisioned") {
						storageClass = AzureFilePremiumStorageClass
					}

					if storageClass != "" {
						var priceInUsd float64

						if len(v.MeterRates) < 1 {
							log.Warnf("missing rate info %+v", map[string]interface{}{"MeterSubCategory": *v.MeterSubCategory, "region": region})
							continue
						}
						for _, rate := range v.MeterRates {
							priceInUsd += *rate
						}
						// rate is in disk per month, resolve price per hour, then GB per hour
						pricePerHour := priceInUsd / 730.0 / 32.0
						priceStr := fmt.Sprintf("%f", pricePerHour)

						key := region + "," + storageClass
						log.Debugf("Adding PV.Key: %s, Cost: %s", key, priceStr)
						allPrices[key] = &AzurePricing{
							PV: &PV{
								Cost:   priceStr,
								Region: region,
							},
						}
					}
				}
			}

			if strings.Contains(meterCategory, "Virtual Machines") {

				usageType := ""
				if !strings.Contains(meterName, "Low Priority") {
					usageType = "ondemand"
				} else {
					usageType = "preemptible"
				}

				var instanceTypes []string
				name := strings.TrimSuffix(meterName, " Low Priority")
				instanceType := strings.Split(name, "/")
				for _, it := range instanceType {
					if strings.Contains(meterSubCategory, "Promo") {
						it = it + " Promo"
					}
					instanceTypes = append(instanceTypes, strings.Replace(it, " ", "_", 1))
				}

				instanceTypes = transformMachineType(meterSubCategory, instanceTypes)
				if strings.Contains(name, "Expired") {
					instanceTypes = []string{}
				}

				var priceInUsd float64

				if len(v.MeterRates) < 1 {
					log.Warnf("missing rate info %+v", map[string]interface{}{"MeterSubCategory": *v.MeterSubCategory, "region": region})
					continue
				}
				for _, rate := range v.MeterRates {
					priceInUsd += *rate
				}
				priceStr := fmt.Sprintf("%f", priceInUsd)
				for _, instanceType := range instanceTypes {

					key := fmt.Sprintf("%s,%s,%s", region, instanceType, usageType)

					allPrices[key] = &AzurePricing{
						Node: &Node{
							Cost:         priceStr,
							BaseCPUPrice: baseCPUPrice,
							UsageType:    usageType,
						},
					}
				}
			}
		}
	}

	// There is no easy way of supporting Standard Azure-File, because it's billed per used GB
	// this will set the price to "0" as a workaround to not spam with `Persistent Volume pricing not found for` error
	// check https://github.com/opencost/opencost/issues/159 for more information (same problem on AWS)
	zeroPrice := "0.0"
	for region := range regions {
		key := region + "," + AzureFileStandardStorageClass
		log.Debugf("Adding PV.Key: %s, Cost: %s", key, zeroPrice)
		allPrices[key] = &AzurePricing{
			PV: &PV{
				Cost:   zeroPrice,
				Region: region,
			},
		}
	}

	az.Pricing = allPrices
	az.RateCardPricingError = nil
	return nil
}

// determineCloudByRegion uses region name to pick the correct Cloud Environment for the azure provider to use
func determineCloudByRegion(region string) azure.Environment {
	lcRegion := strings.ToLower(region)
	if strings.Contains(lcRegion, "china") {
		return azure.ChinaCloud
	}
	if strings.Contains(lcRegion, "gov") || strings.Contains(lcRegion, "dod") {
		return azure.USGovernmentCloud
	}
	// Default to public cloud
	return azure.PublicCloud
}

// NewClientCredentialsConfig creates an AuthorizerConfig object configured to obtain an Authorizer through Client Credentials.
func NewClientCredentialsConfig(clientID string, clientSecret string, tenantID string, env azure.Environment) auth.ClientCredentialsConfig {
	return auth.ClientCredentialsConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TenantID:     tenantID,
		Resource:     env.ResourceManagerEndpoint,
		AADEndpoint:  env.ActiveDirectoryEndpoint,
	}
}

func (az *Azure) addPricing(features string, azurePricing *AzurePricing) {
	if az.Pricing == nil {
		az.Pricing = map[string]*AzurePricing{}
	}
	az.Pricing[features] = azurePricing
}

// AllNodePricing returns the Azure pricing objects stored
func (az *Azure) AllNodePricing() (interface{}, error) {
	az.DownloadPricingDataLock.RLock()
	defer az.DownloadPricingDataLock.RUnlock()
	return az.Pricing, nil
}

// NodePricing returns Azure pricing data for a single node
func (az *Azure) NodePricing(key Key) (*Node, error) {
	az.DownloadPricingDataLock.RLock()
	defer az.DownloadPricingDataLock.RUnlock()

	azKey, ok := key.(*azureKey)
	if !ok {
		return nil, fmt.Errorf("azure: NodePricing: key is of type %T", key)
	}
	config, _ := az.GetConfig()
	if slv, ok := azKey.Labels[config.SpotLabel]; ok && slv == config.SpotLabelValue && config.SpotLabel != "" && config.SpotLabelValue != "" {
		features := strings.Split(azKey.Features(), ",")
		region := features[0]
		instance := features[1]
		spotFeatures := fmt.Sprintf("%s,%s,%s", region, instance, "spot")
		if n, ok := az.Pricing[spotFeatures]; ok {
			log.DedupedInfof(5, "Returning pricing for node %s: %+v from key %s", azKey, n, spotFeatures)
			if azKey.isValidGPUNode() {
				n.Node.GPU = "1" // TODO: support multiple GPUs
			}
			return n.Node, nil
		}
		log.Infof("[Info] found spot instance, trying to get retail price for %s: %s, ", spotFeatures, azKey)

		spotCost, err := getRetailPrice(region, instance, config.CurrencyCode, true)
		if err != nil {
			log.DedupedWarningf(5, "failed to retrieve spot retail pricing")
		} else {
			gpu := ""
			if azKey.isValidGPUNode() {
				gpu = "1"
			}
			spotNode := &Node{
				Cost:      spotCost,
				UsageType: "spot",
				GPU:       gpu,
			}

			az.addPricing(spotFeatures, &AzurePricing{
				Node: spotNode,
			})

			return spotNode, nil
		}
	}

	if n, ok := az.Pricing[azKey.Features()]; ok {
		log.Debugf("Returning pricing for node %s: %+v from key %s", azKey, n, azKey.Features())
		if azKey.isValidGPUNode() {
			n.Node.GPU = azKey.GetGPUCount()
		}
		return n.Node, nil
	}
	log.Warnf("no pricing data found for %s: %s", azKey.Features(), azKey)
	c, err := az.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("No default pricing data available")
	}
	if azKey.isValidGPUNode() {
		return &Node{
			VCPUCost:         c.CPU,
			RAMCost:          c.RAM,
			UsesBaseCPUPrice: true,
			GPUCost:          c.GPU,
			GPU:              azKey.GetGPUCount(),
		}, nil
	}
	return &Node{
		VCPUCost:         c.CPU,
		RAMCost:          c.RAM,
		UsesBaseCPUPrice: true,
	}, nil
}

// Stubbed NetworkPricing for Azure. Pull directly from azure.json for now
func (az *Azure) NetworkPricing() (*Network, error) {
	cpricing, err := az.Config.GetCustomPricingData()
	if err != nil {
		return nil, err
	}
	znec, err := strconv.ParseFloat(cpricing.ZoneNetworkEgress, 64)
	if err != nil {
		return nil, err
	}
	rnec, err := strconv.ParseFloat(cpricing.RegionNetworkEgress, 64)
	if err != nil {
		return nil, err
	}
	inec, err := strconv.ParseFloat(cpricing.InternetNetworkEgress, 64)
	if err != nil {
		return nil, err
	}

	return &Network{
		ZoneNetworkEgressCost:     znec,
		RegionNetworkEgressCost:   rnec,
		InternetNetworkEgressCost: inec,
	}, nil
}

// LoadBalancerPricing on Azure, LoadBalancer services correspond to public IPs. For now the pricing of LoadBalancer
// services will be that of a standard static public IP https://azure.microsoft.com/en-us/pricing/details/ip-addresses/.
// Azure still has load balancers which follow the standard pricing scheme based on rules
// https://azure.microsoft.com/en-us/pricing/details/load-balancer/, they are created on a per-cluster basis.
func (azr *Azure) LoadBalancerPricing() (*LoadBalancer, error) {
	return &LoadBalancer{
		Cost: 0.005,
	}, nil
}

type azurePvKey struct {
	Labels                 map[string]string
	StorageClass           string
	StorageClassParameters map[string]string
	DefaultRegion          string
	ProviderId             string
}

func (az *Azure) GetPVKey(pv *v1.PersistentVolume, parameters map[string]string, defaultRegion string) PVKey {
	providerID := ""
	if pv.Spec.AzureDisk != nil {
		providerID = pv.Spec.AzureDisk.DiskName
	}
	return &azurePvKey{
		Labels:                 pv.Labels,
		StorageClass:           pv.Spec.StorageClassName,
		StorageClassParameters: parameters,
		DefaultRegion:          defaultRegion,
		ProviderId:             providerID,
	}
}

func (key *azurePvKey) ID() string {
	return key.ProviderId
}

func (key *azurePvKey) GetStorageClass() string {
	return key.StorageClass
}

func (key *azurePvKey) Features() string {
	storageClass := key.StorageClassParameters["storageaccounttype"]
	storageSKU := key.StorageClassParameters["skuName"]
	if storageClass != "" {
		if strings.EqualFold(storageClass, "Premium_LRS") {
			storageClass = AzureDiskPremiumSSDStorageClass
		} else if strings.EqualFold(storageClass, "StandardSSD_LRS") {
			storageClass = AzureDiskStandardSSDStorageClass
		} else if strings.EqualFold(storageClass, "Standard_LRS") {
			storageClass = AzureDiskStandardStorageClass
		}
	} else {
		if strings.EqualFold(storageSKU, "Premium_LRS") {
			storageClass = AzureFilePremiumStorageClass
		} else if strings.EqualFold(storageSKU, "Standard_LRS") {
			storageClass = AzureFileStandardStorageClass
		}
	}
	if region, ok := util.GetRegion(key.Labels); ok {
		return region + "," + storageClass
	}

	return key.DefaultRegion + "," + storageClass
}

func (*Azure) GetAddresses() ([]byte, error) {
	return nil, nil
}

func (*Azure) GetDisks() ([]byte, error) {
	return nil, nil
}

func (az *Azure) ClusterInfo() (map[string]string, error) {
	remoteEnabled := env.IsRemoteEnabled()

	m := make(map[string]string)
	m["name"] = "Azure Cluster #1"
	c, err := az.GetConfig()
	if err != nil {
		return nil, err
	}
	if c.ClusterName != "" {
		m["name"] = c.ClusterName
	}
	m["provider"] = "Azure"
	m["account"] = az.clusterAccountId
	m["region"] = az.clusterRegion
	m["remoteReadEnabled"] = strconv.FormatBool(remoteEnabled)
	m["id"] = env.GetClusterID()
	return m, nil

}

func (az *Azure) UpdateConfigFromConfigMap(a map[string]string) (*CustomPricing, error) {
	return az.Config.UpdateFromMap(a)
}

func (az *Azure) UpdateConfig(r io.Reader, updateType string) (*CustomPricing, error) {
	return az.Config.Update(func(c *CustomPricing) error {
		if updateType == AzureStorageUpdateType {
			asc := &AzureStorageConfig{}
			err := json.NewDecoder(r).Decode(&asc)
			if err != nil {
				return err
			}

			c.AzureStorageSubscriptionID = asc.SubscriptionId
			c.AzureStorageAccount = asc.AccountName
			if asc.AccessKey != "" {
				c.AzureStorageAccessKey = asc.AccessKey
			}
			c.AzureStorageContainer = asc.ContainerName
			c.AzureContainerPath = asc.ContainerPath
			c.AzureCloud = asc.AzureCloud
		} else {
			defer az.DownloadPricingData()
			a := make(map[string]interface{})
			err := json.NewDecoder(r).Decode(&a)
			if err != nil {
				return err
			}
			for k, v := range a {
				kUpper := strings.Title(k) // Just so we consistently supply / receive the same values, uppercase the first letter.
				vstr, ok := v.(string)
				if ok {
					err := SetCustomPricingField(c, kUpper, vstr)
					if err != nil {
						return err
					}
				} else {
					return fmt.Errorf("type error while updating config for %s", kUpper)
				}
			}
		}
		if env.IsRemoteEnabled() {
			err := UpdateClusterMeta(env.GetClusterID(), c.ClusterName)
			if err != nil {
				return err
			}
		}

		return nil
	})
}
func (az *Azure) GetConfig() (*CustomPricing, error) {
	c, err := az.Config.GetCustomPricingData()
	if err != nil {
		return nil, err
	}
	if c.Discount == "" {
		c.Discount = "0%"
	}
	if c.NegotiatedDiscount == "" {
		c.NegotiatedDiscount = "0%"
	}
	if c.CurrencyCode == "" {
		c.CurrencyCode = "USD"
	}
	if c.AzureBillingRegion == "" {
		c.AzureBillingRegion = "US"
	}
	// Default to pay-as-you-go Durable offer id
	if c.AzureOfferDurableID == "" {
		c.AzureOfferDurableID = "MS-AZR-0003p"
	}
	if c.ShareTenancyCosts == "" {
		c.ShareTenancyCosts = defaultShareTenancyCost
	}
	if c.SpotLabel == "" {
		c.SpotLabel = defaultSpotLabel
	}
	if c.SpotLabelValue == "" {
		c.SpotLabelValue = defaultSpotLabelValue
	}
	return c, nil
}

func (az *Azure) ApplyReservedInstancePricing(nodes map[string]*Node) {

}

func (az *Azure) PVPricing(pvk PVKey) (*PV, error) {
	az.DownloadPricingDataLock.RLock()
	defer az.DownloadPricingDataLock.RUnlock()

	pricing, ok := az.Pricing[pvk.Features()]
	if !ok {
		log.Debugf("Persistent Volume pricing not found for %s: %s", pvk.GetStorageClass(), pvk.Features())
		return &PV{}, nil
	}
	return pricing.PV, nil
}

func (az *Azure) GetLocalStorageQuery(window, offset time.Duration, rate bool, used bool) string {
	return ""
}

func (az *Azure) ServiceAccountStatus() *ServiceAccountStatus {
	return az.serviceAccountChecks.getStatus()
}

const rateCardPricingSource = "Rate Card API"

// PricingSourceStatus returns the status of the rate card api
func (az *Azure) PricingSourceStatus() map[string]*PricingSource {
	sources := make(map[string]*PricingSource)
	errMsg := ""
	if az.RateCardPricingError != nil {
		errMsg = az.RateCardPricingError.Error()
	}
	rcps := &PricingSource{
		Name:    rateCardPricingSource,
		Enabled: true,
		Error:   errMsg,
	}
	if rcps.Error != "" {
		rcps.Available = false
	} else if len(az.Pricing) == 0 {
		rcps.Error = "No Pricing Data Available"
		rcps.Available = false
	} else {
		rcps.Available = true
	}
	sources[rateCardPricingSource] = rcps
	return sources
}

func (*Azure) ClusterManagementPricing() (string, float64, error) {
	return "", 0.0, nil
}

func (az *Azure) CombinedDiscountForNode(instanceType string, isPreemptible bool, defaultDiscount, negotiatedDiscount float64) float64 {
	return 1.0 - ((1.0 - defaultDiscount) * (1.0 - negotiatedDiscount))
}

func (az *Azure) Regions() []string {
	return azureRegions
}

func parseAzureSubscriptionID(id string) string {
	match := azureSubRegex.FindStringSubmatch(id)
	if len(match) >= 2 {
		return match[1]
	}
	// Return empty string if an account could not be parsed from provided string
	return ""
}
