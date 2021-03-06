package errcode

import (
	"testing"
)

func TestTxErr_String(t *testing.T) {
	tests := []struct {
		in   TxErr
		want string
	}{
		{TxErrRejectCheckPoint, "TxErrRejectCheckPoint"},
		{TxErrNoPreviousOut, "Missing inputs"},
		{ScriptCheckInputsBug, "ScriptCheckInputsBug"},
		{TxErrSignRawTransaction, "TxErrSignRawTransaction"},
		{TxErrInvalidIndexOfIn, "TxErrInvalidIndexOfIn"},
		{TxErrPubKeyType, "TxErrPubKeyType"},
	}

	t.Logf("Running %d tests", len(tests))
	for i, test := range tests {
		result := test.in.String()
		if result != test.want {
			t.Errorf("String #%d\n got: %s want: %s", i, result,
				test.want)
			continue
		}
	}

}
