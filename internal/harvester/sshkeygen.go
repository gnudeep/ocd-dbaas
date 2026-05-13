package harvester

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"

	"golang.org/x/crypto/ssh"
)

// SSHKeyPair holds the controller's Ed25519 key for VM access.
// PrivateKeyPEM is stored in the credentials Secret.
// AuthorizedKeyLine goes into the VM's ~/.ssh/authorized_keys via cloud-init.
type SSHKeyPair struct {
	PrivateKeyPEM     string
	AuthorizedKeyLine string
}

// generateSSHKeyPair creates an Ed25519 key pair for controller→VM SSH access.
func generateSSHKeyPair() (*SSHKeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	// Marshal private key to OpenSSH PEM format.
	privPEMBlock, err := ssh.MarshalPrivateKey(priv, "dbaas-controller")
	if err != nil {
		return nil, err
	}
	privPEM := string(pem.EncodeToMemory(privPEMBlock))

	// Marshal public key to OpenSSH authorized_keys format.
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	authLine := string(ssh.MarshalAuthorizedKey(sshPub))

	return &SSHKeyPair{
		PrivateKeyPEM:     privPEM,
		AuthorizedKeyLine: authLine,
	}, nil
}
