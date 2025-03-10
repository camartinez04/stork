//go:build unittest
// +build unittest

package schedule

import (
	"testing"
	"time"

	stork_api "github.com/libopenstorage/stork/pkg/apis/stork/v1alpha1"
	fakeclient "github.com/libopenstorage/stork/pkg/client/clientset/versioned/fake"
	"github.com/portworx/sched-ops/k8s/core"
	storkops "github.com/portworx/sched-ops/k8s/stork"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetes "k8s.io/client-go/kubernetes/fake"
)

var fakeStorkClient *fakeclient.Clientset

func TestSchedule(t *testing.T) {
	scheme := runtime.NewScheme()
	err := stork_api.AddToScheme(scheme)
	require.NoError(t, err, "Error adding stork scheme")
	fakeStorkClient = fakeclient.NewSimpleClientset()
	fakeKubeClient := kubernetes.NewSimpleClientset()

	core.SetInstance(core.New(fakeKubeClient))
	storkops.SetInstance(storkops.New(fakeKubeClient, fakeStorkClient, nil))

	t.Run("createDefaultPoliciesTest", createDefaultPoliciesTest)
	t.Run("triggerIntervalRequiredTest", triggerIntervalRequiredTest)
	t.Run("triggerDailyRequiredTest", triggerDailyRequiredTest)
	t.Run("triggerWeeklyRequiredTest", triggerWeeklyRequiredTest)
	t.Run("triggerMonthlyRequiredTest", triggerMonthlyRequiredTest)
	t.Run("validateSchedulePolicyTest", validateSchedulePolicyTest)
	t.Run("policyRetainTest", policyRetainTest)
	t.Run("policyOptionsTest", policyOptionsTest)
}

func createDefaultPoliciesTest(t *testing.T) {
	err := createDefaultPolicy()
	require.NoError(t, err, "Error creating default policies")
	err = createDefaultPolicy()
	require.NoError(t, err, "Error recreating default policies")
	schedulePolicy, err := storkops.Instance().GetSchedulePolicy("default-migration-policy")
	require.NoError(t, err, "Error getting default-migration-policy")
	schedulePolicy.Policy.Interval.IntervalMinutes = 1
	_, err = storkops.Instance().UpdateSchedulePolicy(schedulePolicy)
	require.NoError(t, err, "Error updating default-migration-policy")
	err = createDefaultPolicy()
	require.NoError(t, err, "Error recreating default policies after modifying default-migration-policy")
	schedulePolicy, err = storkops.Instance().GetSchedulePolicy("default-migration-policy")
	require.NoError(t, err, "Error getting default-migration-policy")
	require.Equal(t, 30, schedulePolicy.Policy.Interval.IntervalMinutes, "Error updating the existing default-migration-policy's interval time")
}

func triggerIntervalRequiredTest(t *testing.T) {
	defer func() {
		err := storkops.Instance().DeleteSchedulePolicy("intervalpolicy")
		require.NoError(t, err, "Error cleaning up schedule policy")
	}()

	_, err := storkops.Instance().CreateSchedulePolicy(&stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "intervalpolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Interval: &stork_api.IntervalPolicy{
				IntervalMinutes: 60,
			},
		},
	})
	require.NoError(t, err, "Error creating policy")

	var latestMigrationTimestamp meta.Time
	required, err := TriggerRequired("intervalpolicy", "default", stork_api.SchedulePolicyTypeInterval, latestMigrationTimestamp)
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")

	_, err = TriggerRequired("missingpolicy", "default", stork_api.SchedulePolicyTypeInterval, meta.Date(2019, time.February, 7, 23, 14, 0, 0, time.Local))
	require.Error(t, err, "Should return error for missing policy")

	mockNow := time.Date(2019, time.February, 7, 23, 16, 0, 0, time.Local)
	setMockTime(&mockNow)
	// Last triggered 2 mins ago
	required, err = TriggerRequired("intervalpolicy", "default", stork_api.SchedulePolicyTypeInterval, meta.Date(2019, time.February, 7, 23, 14, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")
	// Last triggered 59 mins ago
	required, err = TriggerRequired("intervalpolicy", "default", stork_api.SchedulePolicyTypeInterval, meta.Date(2019, time.February, 7, 22, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")
	// Last triggered 61 mins ago
	required, err = TriggerRequired("intervalpolicy", "default", stork_api.SchedulePolicyTypeInterval, meta.Date(2019, time.February, 7, 22, 14, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")
}

func triggerDailyRequiredTest(t *testing.T) {
	defer func() {
		err := storkops.Instance().DeleteSchedulePolicy("dailypolicy")
		require.NoError(t, err, "Error cleaning up schedule policy")
	}()

	_, err := storkops.Instance().CreateSchedulePolicy(&stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "dailypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Daily: &stork_api.DailyPolicy{
				Time: "11:15PM",
			},
		},
	})
	require.NoError(t, err, "Error creating policy")

	_, err = TriggerRequired("missingpolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 7, 23, 14, 0, 0, time.Local))
	require.Error(t, err, "Should return error for missing policy")

	mockNow := time.Date(2019, time.February, 7, 23, 16, 0, 0, time.Local)
	setMockTime(&mockNow)
	// Last triggered before schedule
	required, err := TriggerRequired("dailypolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 7, 23, 14, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")

	// Last triggered at schedule
	required, err = TriggerRequired("dailypolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 7, 23, 15, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")

	// Last triggered one day ago at schedule
	required, err = TriggerRequired("dailypolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 6, 23, 15, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")

	// Last triggered one day ago before schedule
	required, err = TriggerRequired("dailypolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 6, 23, 14, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")

	// Last triggered one day ago after schedule
	required, err = TriggerRequired("dailypolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 6, 23, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")

	// Set time two hours before next day's schedule
	mockNow = time.Date(2019, time.February, 8, 21, 15, 0, 0, time.Local)
	setMockTime(&mockNow)

	// Last triggered one day ago at schedule
	required, err = TriggerRequired("dailypolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 7, 23, 15, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")

	// Last triggered one day ago after schedule
	required, err = TriggerRequired("dailypolicy", "default", stork_api.SchedulePolicyTypeDaily, meta.Date(2019, time.February, 7, 23, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")
}

func triggerWeeklyRequiredTest(t *testing.T) {
	defer func() {
		err := storkops.Instance().DeleteSchedulePolicy("weeklypolicy")
		require.NoError(t, err, "Error cleaning up schedule policy")
	}()

	_, err := storkops.Instance().CreateSchedulePolicy(&stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "weeklypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Weekly: &stork_api.WeeklyPolicy{
				Day:  "Sunday",
				Time: "11:15pm",
			},
		},
	})
	require.NoError(t, err, "Error creating policy")

	_, err = TriggerRequired("missingpolicy", "default", stork_api.SchedulePolicyTypeWeekly, meta.Date(2019, time.February, 7, 23, 14, 0, 0, time.Local))
	require.Error(t, err, "Should return error for missing policy")

	newTime := time.Date(2019, time.February, 7, 23, 16, 0, 0, time.Local) // Current day: Thursday
	setMockTime(&newTime)
	// LastTriggered one week before on Saturday at 11:15pm
	required, err := TriggerRequired("weeklypolicy", "default", stork_api.SchedulePolicyTypeWeekly, meta.Date(2019, time.February, 2, 23, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")

	// LastTriggered one week before on Sunday at 11:15pm
	required, err = TriggerRequired("weeklypolicy", "default", stork_api.SchedulePolicyTypeWeekly, meta.Date(2019, time.February, 3, 23, 15, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")

	newTime = time.Date(2019, time.February, 10, 23, 16, 0, 0, time.Local) // Current date: Sunday 11:16pm
	setMockTime(&newTime)
	// LastTriggered last Wednesday at 11:16pm
	required, err = TriggerRequired("weeklypolicy", "default", stork_api.SchedulePolicyTypeWeekly, meta.Date(2019, time.February, 6, 23, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")

	newTime = time.Date(2019, time.February, 17, 23, 15, 0, 0, time.Local) // Current day: Sunday
	setMockTime(&newTime)
	// LastTriggered last Sunday at 11:16pm
	required, err = TriggerRequired("weeklypolicy", "default", stork_api.SchedulePolicyTypeWeekly, meta.Date(2019, time.February, 10, 23, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")
}

func triggerMonthlyRequiredTest(t *testing.T) {
	_, err := storkops.Instance().CreateSchedulePolicy(&stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "monthlypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Monthly: &stork_api.MonthlyPolicy{
				Date: 28,
				Time: "11:15pm",
			},
		},
	})
	require.NoError(t, err, "Error creating policy")

	_, err = TriggerRequired("missingpolicy", "default", stork_api.SchedulePolicyTypeMonthly, meta.Date(2019, time.February, 7, 23, 14, 0, 0, time.Local))
	require.Error(t, err, "Should return error for missing policy")

	newTime := time.Date(2019, time.February, 28, 23, 16, 0, 0, time.Local)
	setMockTime(&newTime)
	// Last triggered before schedule
	required, err := TriggerRequired("monthlypolicy", "default", stork_api.SchedulePolicyTypeMonthly, meta.Date(2019, time.February, 2, 23, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.True(t, required, "Trigger should have been required")

	// Last triggered one minute after schedule
	required, err = TriggerRequired("monthlypolicy", "default", stork_api.SchedulePolicyTypeMonthly, meta.Date(2019, time.February, 28, 23, 16, 0, 0, time.Local))
	require.NoError(t, err, "Error checking if trigger required")
	require.False(t, required, "Trigger should not have been required")
}

func validateSchedulePolicyTest(t *testing.T) {
	policy := &stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "validpolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Daily: &stork_api.DailyPolicy{
				Time: "01:15am",
			},
			Weekly: &stork_api.WeeklyPolicy{
				Day:  "Sunday",
				Time: "11:15pm",
			},
			Monthly: &stork_api.MonthlyPolicy{
				Date: 15,
				Time: "12:15pm",
			},
		},
	}
	err := ValidateSchedulePolicy(policy)
	require.NoError(t, err, "Valid policy shouldn't return error")

	policy = &stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "invalidintervalpolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Interval: &stork_api.IntervalPolicy{},
		},
	}
	err = ValidateSchedulePolicy(policy)
	require.Error(t, err, "Invalid interval policy should return error")

	policy = &stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "invaliddailypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Daily: &stork_api.DailyPolicy{
				Time: "25:15am",
			},
		},
	}
	err = ValidateSchedulePolicy(policy)
	require.Error(t, err, "Invalid daily policy should return error")

	policy = &stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "invalidweeklypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Weekly: &stork_api.WeeklyPolicy{
				Day:  "T",
				Time: "11:15pm",
			},
		},
	}
	err = ValidateSchedulePolicy(policy)
	require.Error(t, err, "Invalid weekly policy should return error")

	policy = &stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "invalidweeklypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Weekly: &stork_api.WeeklyPolicy{
				Day:  "Tue",
				Time: "13:15pm",
			},
		},
	}
	err = ValidateSchedulePolicy(policy)
	require.Error(t, err, "Invalid weekly policy should return error")

	policy = &stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "invalidMonthlypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Monthly: &stork_api.MonthlyPolicy{
				Date: 32,
				Time: "11:15pm",
			},
		},
	}
	err = ValidateSchedulePolicy(policy)
	require.Error(t, err, "Invalid monthly policy should return error")

	policy = &stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: "invalidMonthlypolicy",
		},
		Policy: stork_api.SchedulePolicyItem{
			Monthly: &stork_api.MonthlyPolicy{
				Date: 12,
				Time: "13:15pm",
			},
		},
	}
	err = ValidateSchedulePolicy(policy)
	require.Error(t, err, "Invalid monthly policy should return error")
}

func policyRetainTest(t *testing.T) {
	policyName := "policy"
	policy, err := storkops.Instance().CreateSchedulePolicy(&stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: policyName,
		},
		Policy: stork_api.SchedulePolicyItem{
			Interval: &stork_api.IntervalPolicy{
				IntervalMinutes: 60,
			},
			Daily: &stork_api.DailyPolicy{
				Time: "10:40PM",
			},
			Weekly: &stork_api.WeeklyPolicy{
				Time: "10:40PM",
				Day:  "Thur",
			},
			Monthly: &stork_api.MonthlyPolicy{
				Time: "10:40PM",
				Date: 25,
			},
		},
	})
	require.NoError(t, err, "Error creating schedule policy")

	retain, err := GetRetain(policyName, "default", stork_api.SchedulePolicyTypeInterval)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultIntervalPolicyRetain, retain, "Wrong default retain for interval policy")
	policy.Policy.Interval.Retain = 0
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")
	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeInterval)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultIntervalPolicyRetain, retain, "Wrong default retain for interval policy")

	policy.Policy.Interval.Retain = 5
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")

	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeInterval)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, policy.Policy.Interval.Retain, retain, "Wrong retain for interval policy")

	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeDaily)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultDailyPolicyRetain, retain, "Wrong default retain for daily policy")
	policy.Policy.Daily.Retain = 0
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")
	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeDaily)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultDailyPolicyRetain, retain, "Wrong default retain for daily policy")

	policy.Policy.Daily.Retain = 10
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")
	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeDaily)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, policy.Policy.Daily.Retain, retain, "Wrong default retain for daily policy")

	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeWeekly)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultWeeklyPolicyRetain, retain, "Wrong default retain for weekly policy")
	policy.Policy.Weekly.Retain = 0
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")
	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeWeekly)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultWeeklyPolicyRetain, retain, "Wrong default retain for weekly policy")

	policy.Policy.Weekly.Retain = 20
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")
	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeWeekly)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, policy.Policy.Weekly.Retain, retain, "Wrong default retain for weekly policy")

	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeMonthly)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultMonthlyPolicyRetain, retain, "Wrong default retain for monthly policy")
	policy.Policy.Monthly.Retain = 0
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")
	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeMonthly)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, stork_api.DefaultMonthlyPolicyRetain, retain, "Wrong default retain for monthly policy")

	policy.Policy.Monthly.Retain = 30
	_, err = storkops.Instance().UpdateSchedulePolicy(policy)
	require.NoError(t, err, "Error updating schedule policy")
	retain, err = GetRetain(policyName, "default", stork_api.SchedulePolicyTypeMonthly)
	require.NoError(t, err, "Error getting retain")
	require.Equal(t, policy.Policy.Monthly.Retain, retain, "Wrong default retain for monthly policy")
}

func policyOptionsTest(t *testing.T) {
	policyName := "options"
	policy, err := storkops.Instance().CreateSchedulePolicy(&stork_api.SchedulePolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: policyName,
		},
		Policy: stork_api.SchedulePolicyItem{
			Interval: &stork_api.IntervalPolicy{
				IntervalMinutes: 60,
				Options: map[string]string{
					"interval-option": "true",
				},
			},
			Daily: &stork_api.DailyPolicy{
				Time: "10:40PM",
				Options: map[string]string{
					"daily-option": "true",
				},
			},
			Weekly: &stork_api.WeeklyPolicy{
				Time: "10:40PM",
				Day:  "Thur",
				Options: map[string]string{
					"weekly-option": "true",
				},
			},
			Monthly: &stork_api.MonthlyPolicy{
				Time: "10:40PM",
				Date: 25,
				Options: map[string]string{
					"monthly-option": "true",
				},
			},
		},
	})
	require.NoError(t, err, "Error creating schedule policy")

	options, err := GetOptions(policyName, "default", stork_api.SchedulePolicyTypeInterval)
	require.NoError(t, err, "Error getting options")
	require.Equal(t, policy.Policy.Interval.Options, options, "Options mismatch for interval policy")
	options, err = GetOptions(policyName, "default", stork_api.SchedulePolicyTypeDaily)
	require.NoError(t, err, "Error getting options")
	require.Equal(t, policy.Policy.Daily.Options, options, "Options mismatch for daily policy")
	options, err = GetOptions(policyName, "default", stork_api.SchedulePolicyTypeWeekly)
	require.NoError(t, err, "Error getting options")
	require.Equal(t, policy.Policy.Weekly.Options, options, "Options mismatch for weekly policy")
	options, err = GetOptions(policyName, "default", stork_api.SchedulePolicyTypeMonthly)
	require.NoError(t, err, "Error getting options")
	require.Equal(t, policy.Policy.Monthly.Options, options, "Options mismatch for monthly policy")
}
