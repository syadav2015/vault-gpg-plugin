package gpg

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"
	"io"
	"strings"
)

func pathListKeys(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "keys/?$",
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ListOperation: &framework.PathOperation{
				Callback: b.pathKeyList,
			},
		},
		HelpSynopsis:    pathPolicyHelpSyn,
		HelpDescription: pathPolicyHelpDesc,
	}
}

func pathKeys(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "keys/" + framework.GenericNameRegex("name"),
		Fields: map[string]*framework.FieldSchema{
			"name": {
				Type:        framework.TypeString,
				Description: "Name of the key.",
			},
			"real_name": {
				Type:        framework.TypeString,
				Description: "The real name of the identity associated with the generated GPG key. Must not contain any of \"()<>\x00\". Only used if generate is false.",
			},
			"email": {
				Type:        framework.TypeString,
				Description: "The email of the identity associated with the generated GPG key. Must not contain any of \"()<>\x00\". Only used if generate is false.",
			},
			"comment": {
				Type:        framework.TypeString,
				Description: "The comment of the identity associated with the generated GPG key. Must not contain any of \"()<>\x00\". Only used if generate is false.",
			},
			"key_bits": {
				Type:        framework.TypeInt,
				Default:     2048,
				Description: "The number of bits to use. Only used if generate is true.",
			},
			"key": {
				Type:        framework.TypeString,
				Description: "The ASCII-armored GPG key to use. Only used if generate is false.",
			},
			"exportable": {
				Type:        framework.TypeBool,
				Description: "Enables the key to be exportable.",
			},
			"generate": {
				Type:        framework.TypeBool,
				Default:     true,
				Description: "Determines if a key should be generated by Vault or if a key is being passed from another service.",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.pathKeyRead,
			},
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathKeyCreate,
			},
			logical.DeleteOperation: &framework.PathOperation{
				Callback: b.pathKeyDelete,
			},
		},
		HelpSynopsis:    pathPolicyHelpSyn,
		HelpDescription: pathPolicyHelpDesc,
	}
}

func (b *backend) key(ctx context.Context, s logical.Storage, name string) (*keyEntry, error) {
	entry, err := s.Get(ctx, "key/"+name)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}

	var result keyEntry
	if err := entry.DecodeJSON(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

func (b *backend) entity(entry *keyEntry) (*openpgp.Entity, error) {
	r := bytes.NewReader(entry.SerializedKey)
	el, err := openpgp.ReadKeyRing(r)
	if err != nil {
		return nil, err
	}

	return el[0], nil
}

func serializePrivateWithoutSigning(w io.Writer, e *openpgp.Entity) (err error) {
	foundPrivateKey := false

	if e.PrivateKey != nil {
		foundPrivateKey = true
		err = e.PrivateKey.Serialize(w)
		if err != nil {
			return
		}
	}
	for _, ident := range e.Identities {
		err = ident.UserId.Serialize(w)
		if err != nil {
			return
		}
		err = ident.SelfSignature.Serialize(w)
		if err != nil {
			return
		}
	}
	for _, subkey := range e.Subkeys {
		if subkey.PrivateKey != nil {
			foundPrivateKey = true
			err = subkey.PrivateKey.Serialize(w)
			if err != nil {
				return
			}
		}
		err = subkey.Sig.Serialize(w)
		if err != nil {
			return
		}
	}

	if !foundPrivateKey {
		return fmt.Errorf("No private key has been found")
	}

	return nil
}

func (b *backend) pathKeyRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	entry, err := b.key(ctx, req.Storage, data.Get("name").(string))
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}
	entity, err := b.entity(entry)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	err = entity.Serialize(w)
	w.Close()
	if err != nil {
		return nil, err
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"fingerprint": hex.EncodeToString(entity.PrimaryKey.Fingerprint[:]),
			"public_key":  buf.String(),
			"exportable":  entry.Exportable,
		},
	}, nil
}

func (b *backend) pathKeyCreate(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	name := data.Get("name").(string)
	realName := data.Get("real_name").(string)
	email := data.Get("email").(string)
	comment := data.Get("comment").(string)
	keyBits := data.Get("key_bits").(int)
	exportable := data.Get("exportable").(bool)
	generate := data.Get("generate").(bool)
	key := data.Get("key").(string)

	var buf bytes.Buffer
	switch generate {
	case true:
		if keyBits < 2048 {
			return logical.ErrorResponse("Keys < 2048 bits are unsafe and not supported"), nil
		}
		config := packet.Config{
			RSABits: keyBits,
		}
		entity, err := openpgp.NewEntity(realName, comment, email, &config)
		if err != nil {
			return nil, err
		}
		err = entity.SerializePrivate(&buf, nil)
		if err != nil {
			return nil, err
		}
	default:
		if key == "" {
			return logical.ErrorResponse("the key value is required for generated keys"), nil
		}
		el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(key))
		if err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}
		err = serializePrivateWithoutSigning(&buf, el[0])
		if err != nil {
			return logical.ErrorResponse("the key could not be serialized, is a private key present?"), nil
		}
	}

	entry, err := logical.StorageEntryJSON("key/"+name, &keyEntry{
		SerializedKey: buf.Bytes(),
		Exportable:    exportable,
	})
	if err != nil {
		return nil, err
	}
	if err := req.Storage.Put(ctx, entry); err != nil {
		return nil, err
	}
	return nil, nil
}

func (b *backend) pathKeyDelete(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	err := req.Storage.Delete(ctx, "key/"+data.Get("name").(string))
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (b *backend) pathKeyList(
	ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	entries, err := req.Storage.List(ctx, "key/")
	if err != nil {
		return nil, err
	}
	return logical.ListResponse(entries), nil
}

type keyEntry struct {
	SerializedKey []byte
	Exportable    bool
}

const pathPolicyHelpSyn = "Managed named GPG keys"
const pathPolicyHelpDesc = `
This path is used to manage the named GPG keys that are available.
Doing a write with no value against a new named key will create
it using a randomly generated key.
`
