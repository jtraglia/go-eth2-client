// Copyright © 2024 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package electra

import (
	"bytes"
	"fmt"

	"github.com/goccy/go-yaml"
)

// depositReceiptYAML is the spec representation of the struct.
type depositReceiptYAML struct {
	Pubkey                string `yaml:"pubkey"`
	WithdrawalCredentials string `yaml:"withdrawal_credentials"`
	Amount                uint64 `yaml:"amount"`
	Signature             string `yaml:"signature"`
	Index                 uint64 `yaml:"index"`
}

// MarshalYAML implements yaml.Marshaler.
func (d *DepositReceipt) MarshalYAML() ([]byte, error) {
	yamlBytes, err := yaml.MarshalWithOptions(&depositReceiptYAML{
		Pubkey:                fmt.Sprintf("%#x", d.Pubkey),
		WithdrawalCredentials: fmt.Sprintf("%#x", d.WithdrawalCredentials),
		Amount:                uint64(d.Amount),
		Signature:             fmt.Sprintf("%#x", d.Signature),
		Index:                 d.Index,
	}, yaml.Flow(true))
	if err != nil {
		return nil, err
	}

	return bytes.ReplaceAll(yamlBytes, []byte(`"`), []byte(`'`)), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *DepositReceipt) UnmarshalYAML(input []byte) error {
	// We unmarshal to the JSON struct to save on duplicate code.
	var depositReceipt depositReceiptJSON
	if err := yaml.Unmarshal(input, &depositReceipt); err != nil {
		return err
	}

	return d.unpack(&depositReceipt)
}
