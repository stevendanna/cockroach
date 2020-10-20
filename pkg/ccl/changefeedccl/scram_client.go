// Copyright 2018 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl

import (
	"hash"

	"github.com/Shopify/sarama"
	"github.com/xdg/scram"
)

var (
	sha256 scram.HashGeneratorFcn = func() hash.Hash { return sha256.New() }
	sha512 scram.HashGeneratorFcn = func() hash.Hash { return sha512.New() }

	// SHA256ClientGenerator returns a SCRAMClient for the
	// SCRAM-SHA-256 SASL mechanism. This can used as a
	// SCRAMCLientGeneratorFunc when constructing a sarama SASL
	// configuration.
	sha256ClientGenerator = func() sarama.SCRAMClient {
		return &scramClient{HashGeneratorFcn: sha256}
	}

	// sha512ClientGenerator returns a SCRAMClient for the
	// SCRAM-SHA-512 SASL mechanism. This can used as a
	// SCRAMCLientGeneratorFunc when constructing a sarama SASL
	// configuration.
	sha512ClientGenerator = func() sarama.SCRAMClient {
		return &scramClient{HashGeneratorFcn: sha512}
	}
)

type scramClient struct {
	*scram.Client
	*scram.ClientConversation
	scram.HashGeneratorFcn
}

var _ sarama.SCRAMClient = &scramClient{}

func (c *scramClient) Begin(userName, password, authzID string) error {
	var err error
	c.Client, err = c.HashGeneratorFcn.NewClient(userName, password, authzID)
	if err != nil {
		return err
	}
	c.ClientConversation = c.Client.NewConversation()
	return nil
}

func (c *scramClient) Step(challenge string) (string, error) {
	return c.ClientConversation.Step(challenge)
}

func (c *scramClient) Done() bool {
	return c.ClientConversation.Done()
}
