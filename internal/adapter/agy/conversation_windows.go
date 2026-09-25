//go:build windows

package agy

// Windows has no ownership/mode identity for the conversation file and
// no evidence (AC-010 spec §3.12): inspection always fails closed.

import "errors"

var errConversationInspectionUnsupported = errors.New("agy conversation-file inspection is unsupported on windows (spec §3.12)")

func conversationFileIdentity(string) (string, error) {
	return "", errConversationInspectionUnsupported
}

func verifyConversationFile(string, string) error {
	return errConversationInspectionUnsupported
}
