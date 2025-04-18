package gateway

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpdateConditionIfChanged(t *testing.T) {
	now := metav1.Now()
	past := metav1.NewTime(now.Add(-5 * time.Minute))

	tests := []struct {
		name               string
		existingConditions []metav1.Condition
		newCondition       metav1.Condition
		wantConditions     []metav1.Condition
		wantChanged        bool
	}{
		{
			name:               "add new condition when none exist",
			existingConditions: []metav1.Condition{},
			newCondition: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "Initial",
			},
			wantConditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "Initial",
					// LastTransitionTime will be set by the function
				},
			},
			wantChanged: true,
		},
		{
			name: "no change to existing condition",
			existingConditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Exists",
					Message:            "Already here",
					LastTransitionTime: past,
				},
			},
			newCondition: metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "Exists",
				Message: "Already here",
			},
			wantConditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Exists",
					Message:            "Already here",
					LastTransitionTime: past, // Should not change
				},
			},
			wantChanged: false,
		},
		{
			name: "status change in existing condition",
			existingConditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					Reason:             "OldReason",
					LastTransitionTime: past,
				},
			},
			newCondition: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue, // Status changed
				Reason: "OldReason",
			},
			wantConditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "OldReason",
					// LastTransitionTime will be updated
				},
			},
			wantChanged: true,
		},
		{
			name: "reason change in existing condition",
			existingConditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "OldReason",
					LastTransitionTime: past,
				},
			},
			newCondition: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "NewReason", // Reason changed
			},
			wantConditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "NewReason",
					// LastTransitionTime will be updated
				},
			},
			wantChanged: true,
		},
		{
			name: "add new condition when others exist",
			existingConditions: []metav1.Condition{
				{Type: "Synced", Status: metav1.ConditionTrue, Reason: "A", LastTransitionTime: past},
			},
			newCondition: metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "B"},
			wantConditions: []metav1.Condition{
				{Type: "Synced", Status: metav1.ConditionTrue, Reason: "A", LastTransitionTime: past},
				{Type: "Ready", Status: metav1.ConditionFalse, Reason: "B"}, // LastTransitionTime will be set
			},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make copies to avoid modifying originals
			existingCopy := make([]metav1.Condition, len(tt.existingConditions))
			copy(existingCopy, tt.existingConditions)

			gotConditions, gotChanged := UpdateConditionIfChanged(existingCopy, tt.newCondition)

			if gotChanged != tt.wantChanged {
				t.Errorf("UpdateConditionIfChanged() gotChanged = %v, want %v", gotChanged, tt.wantChanged)
			}

			// Clear LastTransitionTime for comparison where change is expected
			if tt.wantChanged {
				for i := range gotConditions {
					if gotConditions[i].Type == tt.newCondition.Type {
						gotConditions[i].LastTransitionTime = metav1.Time{} // Zero out for comparison
						tt.wantConditions[i].LastTransitionTime = metav1.Time{}
					}
				}
			}

			if !reflect.DeepEqual(gotConditions, tt.wantConditions) {
				t.Errorf("UpdateConditionIfChanged() gotConditions = %v, want %v", gotConditions, tt.wantConditions)
			}
		})
	}
}
