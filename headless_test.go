package main

import (
	"testing"

	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestCreationQuestionsDeclareHeadlessDefaults(t *testing.T) {
	builder := NewBuilder()
	builder.Base.Identity = &resources.ServiceIdentity{Module: "app", Name: "database"}
	foundMigration := false
	for _, question := range builder.Options() {
		answer, err := communicate.AnswerDefault(question)
		require.NoError(t, err, "question %s", question.GetMessage().GetName())
		if question.GetMessage().GetName() == MigrationFormat {
			foundMigration = true
			require.Equal(t, builder.Settings.MigrationFormat, answer.GetChoice().GetOption())
			require.Equal(t, "gomigrate", answer.GetChoice().GetOption())
		}
	}
	require.True(t, foundMigration)
}
