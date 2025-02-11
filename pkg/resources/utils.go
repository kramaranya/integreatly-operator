package resources

import (
	integreatlyv1alpha1 "github.com/integr8ly/integreatly-operator/apis/v1alpha1"
)

func IsInProw(inst *integreatlyv1alpha1.RHMI) bool {
	annotationMap := inst.GetObjectMeta().GetAnnotations()
	isInProw, ok := annotationMap["in_prow"]
	if ok && isInProw == "true" {
		return true
	}
	return false
}
