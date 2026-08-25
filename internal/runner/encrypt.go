package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
)

// EncryptArtifacts encrypts the mandatory private log and an optional IPA,
// then removes every plaintext input even if encryption fails. The output
// directory contains only build.log.age and, on success, App.ipa.age.
func EncryptArtifacts(recipientText, logPath, ipaPath, outputDir string) error {
	return EncryptArtifactsWithRecipients(recipientText, recipientText, logPath, ipaPath, outputDir)
}

// EncryptArtifactsWithRecipients keeps diagnostics decryptable by the local
// CLI while allowing a TestFlight intermediate IPA to be encrypted to a
// distinct identity held only by the protected signing Environment.
func EncryptArtifactsWithRecipients(logRecipientText, ipaRecipientText, logPath, ipaPath, outputDir string) error {
	return encryptArtifactsWithRecipientsNamed(logRecipientText, ipaRecipientText, logPath, ipaPath, outputDir, "App.ipa.age")
}

func encryptArtifactsWithRecipientsNamed(logRecipientText, ipaRecipientText, logPath, ipaPath, outputDir, ipaName string) error {
	if ipaName != "App.ipa.age" && ipaName != "project-output.age" {
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("invalid encrypted IPA output name")
	}
	logRecipient, err := age.ParseX25519Recipient(logRecipientText)
	if err != nil {
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("parse diagnostic AGE recipient")
	}
	ipaRecipient, err := age.ParseX25519Recipient(ipaRecipientText)
	if err != nil {
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("parse IPA AGE recipient")
	}
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("create encrypted artifact directory")
	}
	info, err := os.Lstat(outputDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(outputDir) // removes a symlink itself, never its target
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("encrypted artifact path is not a private directory")
	}
	if err := os.Chmod(outputDir, 0700); err != nil {
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("secure encrypted artifact directory")
	}
	if _, err := os.ReadDir(outputDir); err != nil {
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("inspect encrypted artifact directory")
	}
	for _, name := range []string{"build.log.age", "build.log.age.partial", "App.ipa.age", "App.ipa.age.partial", "project-output.age", "project-output.age.partial"} {
		if err := os.RemoveAll(filepath.Join(outputDir, name)); err != nil {
			removePlaintext(logPath, ipaPath)
			return fmt.Errorf("clean encrypted artifact destination")
		}
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 0 {
		removePlaintext(logPath, ipaPath)
		return fmt.Errorf("encrypted artifact directory is not empty")
	}
	if err := encryptAndRemove(logRecipient, logPath, filepath.Join(outputDir, "build.log.age"), false); err != nil {
		removePlaintext(ipaPath)
		return err
	}
	if ipaPath != "" {
		if _, statErr := os.Stat(ipaPath); statErr == nil {
			if err := encryptAndRemove(ipaRecipient, ipaPath, filepath.Join(outputDir, ipaName), true); err != nil {
				return err
			}
		} else if !os.IsNotExist(statErr) {
			removePlaintext(ipaPath)
			return fmt.Errorf("inspect plaintext IPA")
		}
	}
	return nil
}

func encryptAndRemove(recipient age.Recipient, sourcePath, destinationPath string, requireNonempty bool) (retErr error) {
	defer func() {
		if err := os.Remove(sourcePath); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = fmt.Errorf("remove plaintext artifact")
		}
	}()
	info, err := os.Lstat(sourcePath)
	if err != nil || !info.Mode().IsRegular() || (requireNonempty && info.Size() == 0) {
		return fmt.Errorf("plaintext artifact is missing or invalid")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open plaintext artifact")
	}
	defer source.Close()
	temporary := destinationPath + ".partial"
	_ = os.Remove(temporary)
	destination, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create encrypted artifact")
	}
	complete := false
	defer func() {
		_ = destination.Close()
		if !complete {
			_ = os.Remove(temporary)
		}
	}()
	writer, err := age.Encrypt(destination, recipient)
	if err != nil {
		return fmt.Errorf("initialize artifact encryption")
	}
	if _, err := io.Copy(writer, source); err != nil {
		return fmt.Errorf("encrypt artifact")
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish artifact encryption")
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("finish encrypted artifact")
	}
	if err := os.Rename(temporary, destinationPath); err != nil {
		return fmt.Errorf("publish encrypted artifact")
	}
	complete = true
	return nil
}

func removePlaintext(paths ...string) {
	for _, path := range paths {
		if path != "" {
			_ = os.Remove(path)
		}
	}
}
