package common

import (
	"fmt"
	"time"

	"github.com/integr8ly/integreatly-operator/apis/v1alpha1"
	integreatlyv1alpha1 "github.com/integr8ly/integreatly-operator/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/util/wait"
)

var (
	managedApiExpectedStageProducts = map[string][]string{
		"installation": {
			"rhsso",
			"cloud-resources",
			"observability",
			"rhssouser",
			"3scale",
			"marin3r",
			"grafana",
		},
	}

	mtManagedApiExpectedStageProducts = map[string][]string{
		"bootstrap": {},
		"installation": {
			"rhsso",
			"cloud-resources",
			"observability",
			"3scale",
			"marin3r",
			"grafana",
		},
	}
)

func TestIntegreatlyStagesStatus(t TestingTB, ctx *TestingContext) {
	err := wait.PollImmediateInfinite(time.Second*15, func() (bool, error) {
		done := true

		//get RHMI
		rhmi, err := GetRHMI(ctx.Client, true)
		if err != nil {
			return false, fmt.Errorf("error getting RHMI CR: %v", err)
		}

		expectedStageProducts := getExpectedStageProducts(rhmi.Spec.Type)

		//iterate stages and check their status
		for stageName, productNames := range expectedStageProducts {
			stage, ok := rhmi.Status.Stages[v1alpha1.StageName(stageName)]
			if !ok {
				t.Errorf("Error checking stage %s. Not found", stageName)
				done = true
				continue
			}

			if status := checkStageStatus(stage); status != "" {
				if retryStatus(status) {
					t.Logf("Status for stage %s in progress. Retrying...", stageName)
					done = false
				} else {
					t.Errorf("Error: Stage %v failed. It's current status is %v", stage.Name, status)
					done = true
				}
			}

			for _, productName := range productNames {
				product, ok := stage.Products[v1alpha1.ProductName(productName)]
				if !ok {
					t.Errorf("Product %s not found in stage %s", productName, stageName)
					done = true
					continue
				}

				if status := checkProductStatus(product); status != "" {
					if retryStatus(status) {
						t.Logf("Status for product %s in stage %s in progress. Retrying...", productName, stageName)
						done = false
					} else {
						t.Errorf("Error: Product %s status failed. It's current status is %s", productName, status)
						done = true
					}
				}
			}
		}

		return done, nil
	})

	if err != nil {
		t.Error(err)
	}
}

func getExpectedStageProducts(installType string) map[string][]string {
	if integreatlyv1alpha1.IsRHOAMMultitenant(integreatlyv1alpha1.InstallationType(installType)) {
		return mtManagedApiExpectedStageProducts
	} else {
		return managedApiExpectedStageProducts
	}
}

func checkStageStatus(stage v1alpha1.RHMIStageStatus) string {
	return checkStatus(stage.Phase)
}

func checkProductStatus(product v1alpha1.RHMIProductStatus) string {
	return checkStatus(product.Phase)
}

// checkStatus verifies that the status is complete. If it is, returns the empty
// string. If it's not, returns the invalid status as a string
func checkStatus(status v1alpha1.StatusPhase) string {
	//check if status is completed or return and error
	if status == "completed" {
		return ""
	}

	return string(status)
}

func retryStatus(status string) bool {
	return status == "in progress"
}
