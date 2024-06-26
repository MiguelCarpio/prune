package main

import (
	"context"
	"log"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

type KeyPair struct {
	resource *KeyPairParser
	client   *gophercloud.ServiceClient
}

func (s KeyPair) CreatedAt() time.Time {
	return s.resource.UpdatedAt
}

func (s KeyPair) Delete(ctx context.Context) error {
	return keypairs.Delete(ctx, s.client, s.ID(), keypairs.DeleteOpts{}).ExtractErr()
}

func (s KeyPair) Type() string {
	return "key"
}

func (s KeyPair) ID() string {
	return s.resource.Name
}

func (s KeyPair) Name() string {
	return s.resource.Name
}

type KeyPairParser struct {
	keypairs.KeyPair
	UpdatedAt time.Time `json:"created_at"`
}

func ListKeyPairs(ctx context.Context, client *gophercloud.ServiceClient) <-chan Resource {
	ch := make(chan Resource)
	go func() {
		defer close(ch)
		if err := keypairs.List(client, nil).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			keypairPage, err := keypairs.ExtractKeyPairs(page)
			if err != nil {
				return true, err
			}
			for i := range keypairPage {
				var k struct {
					KeyPairParser `json:"keypair"`
				}
				if err := keypairs.Get(ctx, client, keypairPage[i].Name, nil).ExtractInto(&k); err != nil {
					return true, err
				}
				log.Println(k)
				ch <- KeyPair{
					resource: &k.KeyPairParser,
					client:   client,
				}
			}
			return true, err
		}); err != nil {
			panic(err)
		}
	}()
	return ch
}
