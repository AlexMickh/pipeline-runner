package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

type flags struct {
	user      string
	password  string
	addr      string
	cfgPath   string
	remoteDir string
}

type Config struct {
	Tasks []Task `yaml:"tasks"`
}

type Task struct {
	Name string   `yaml:"name"`
	Cmds []string `yaml:"cmds"`
}

func main() {
	f := parseFlags()
	cfg, err := parseConfigFile(f.cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	sshConfig := &ssh.ClientConfig{
		User: f.user,
		Auth: []ssh.AuthMethod{
			ssh.Password(f.password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	client, err := ssh.Dial("tcp", f.addr, sshConfig)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	workDir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	if err = deleteDir(client, f.remoteDir, f.user); err != nil {
		log.Fatal(err)
	}

	if err = copyFiles(client, workDir, f.remoteDir); err != nil {
		log.Fatal(err)
	}

	if err = processConfig(context.TODO(), client, cfg, f.remoteDir); err != nil {
		log.Fatal(err)
	}
}

func parseFlags() flags {
	var vars flags

	flag.StringVar(&vars.user, "user", "user1", "Пользователь ВМ")
	flag.StringVar(&vars.password, "password", "123", "Пароль от ВМ")
	flag.StringVar(&vars.addr, "addr", "localhost:8888", "Адрес ВМ")
	flag.StringVar(&vars.cfgPath, "cfg", "local_ci.yml", "Путь до конфига")
	flag.StringVar(&vars.remoteDir, "dir", "$HOME", "Рабочая дериктория на ВМ")

	flag.Parse()

	return vars
}

func parseConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("failed to open config: %w", err)
	}

	var cfg Config
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse config: %w", err)
	}

	return cfg, nil
}

func deleteDir(client *ssh.Client, remoteDir, user string) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create ssh session: %w", err)
	}
	defer session.Close()

	if remoteDir == "/" || remoteDir == "$HOME" || remoteDir == filepath.Join("/home", user) {
		return nil
	}

	cmd := fmt.Sprintf("rm -rf %s", remoteDir)
	if err = session.Start(cmd); err != nil {
		return fmt.Errorf("failed to delete working directory: %w", err)
	}

	return nil
}

func copyFiles(client *ssh.Client, localDir, remoteDir string) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create ssh session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin stdin pipe: %w", err)
	}

	var stderr bytes.Buffer
	session.Stderr = &stderr

	cmd := fmt.Sprintf("mkdir -p %s && tar -xf - -C %s", remoteDir, remoteDir)
	if err = session.Start(cmd); err != nil {
		return fmt.Errorf("failed to execute tar: %w", err)
	}

	tw := tar.NewWriter(stdin)
	err = filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		if err = tw.WriteHeader(header); err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)

		return err
	})

	tw.Close()
	stdin.Close()

	if waitErr := session.Wait(); waitErr != nil {
		return fmt.Errorf("ошибка распаковки на ВМ: %v | stderr: %s", waitErr, stderr.String())
	}

	return err
}

func processConfig(ctx context.Context, client *ssh.Client, cfg Config, remoteDir string) error {
	for _, t := range cfg.Tasks {
		for _, c := range t.Cmds {
			err := func() error {
				session, err := client.NewSession()
				if err != nil {
					return fmt.Errorf("failed to create ssh session: %w", err)
				}
				defer session.Close()

				// cmd := fmt.Sprintf("cd %s", remoteDir)
				// if err = session.Start(cmd); err != nil {
				// 	return fmt.Errorf("failed to cd in working directory: %w", err)
				// }

				var stdout, stderr bytes.Buffer
				session.Stdout = &stdout
				session.Stderr = &stderr

				cmd := fmt.Sprintf("cd %s && %s", remoteDir, c)
				if err = session.Start(cmd); err != nil {
					return fmt.Errorf("failed to start command %s in task %s", c, t.Name)
				}

				done := make(chan error, 1)
				go func() {
					done <- session.Wait()
				}()

				select {
				case <-ctx.Done():
					_ = session.Signal(ssh.SIGKILL)
					return errors.New("stop")
				case err := <-done:
					if err != nil {
						var exitErr *ssh.ExitError
						if errors.As(err, &exitErr) {
							return fmt.Errorf("command %s in task %s finish with code %d", c, t.Name, exitErr.ExitStatus())
						}

						return fmt.Errorf("error in command %s in task %s: %w", c, t.Name, err)
					}

					log.Printf("command %s in task %s:\n%s\n", c, t.Name, stdout.String())
				}

				return nil
			}()
			if err != nil {
				return err
			}
		}
	}

	return nil
}
