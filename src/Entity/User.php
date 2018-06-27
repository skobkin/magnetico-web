<?php

namespace App\Entity;

use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Component\Security\Core\User\UserInterface;

/**
 * @ORM\Table(name="users", schema="users")
 * @ORM\Entity(repositoryClass="App\Repository\UserRepository")
 */
class User implements UserInterface, \Serializable
{
    /**
     * @var int
     *
     * @ORM\Id
     * @ORM\Column(name="id", type="integer")
     * @ORM\GeneratedValue(strategy="AUTO")
     */
    private $id;

    /**
     * @var string
     *
     * @ORM\Column(name="username", type="string", length=25, unique=true)
     */
    private $username;

    /**
     * @var string
     *
     * @ORM\Column(name="password", type="text")
     */
    private $password;

    /**
     * @var string
     *
     * @ORM\Column(name="email", type="string", length=254, unique=true)
     */
    private $email;

    /**
     * @var string[]
     *
     * @ORM\Column(name="roles", type="json")
     */
    private $roles = [];

    /**
     * @var \DateTime
     *
     * @ORM\Column(name="created_at", type="datetime")
     */
    private $createdAt;

    /**
     * @var Invite[]|ArrayCollection
     *
     * @ORM\OneToMany(targetEntity="App\Entity\Invite", mappedBy="user", fetch="EXTRA_LAZY")
     */
    private $invites;

    public function __construct(string $username, string $password, string $email, array $roles = [])
    {
        $this->username = $username;
        $this->password = $password;
        $this->email = $email;
        $this->roles = $roles ?: ['ROLE_USER'];
        $this->createdAt = new \DateTime();
    }

    public function getId(): int
    {
        return $this->id;
    }

    public function getUsername()
    {
        return $this->username;
    }

    public function getPassword()
    {
        return $this->password;
    }

    public function updatePassword(string $password): void
    {
        $this->password = $password;
    }

    public function getSalt()
    {
        // Salt is not needed when using Argon2i
        // @see https://symfony.com/doc/current/reference/configuration/security.html#using-the-argon2i-password-encoder
        return null;
    }

    public function getEmail(): string
    {
        return $this->email;
    }

    public function getRoles(): array
    {
        return $this->roles;
    }

    public function addRole(string $role): void
    {
        $this->roles[] = $role;
    }

    public function getCreatedAt(): \DateTime
    {
        return $this->createdAt;
    }

    public function eraseCredentials()
    {

    }

    /** @return Invite[]|ArrayCollection */
    public function getInvites(): \Traversable
    {
        return $this->invites;
    }

    /** @see \Serializable::serialize() */
    public function serialize()
    {
        return serialize([
            $this->id,
            $this->username,
            $this->password,
        ]);
    }

    /** @see \Serializable::unserialize() */
    public function unserialize($serialized)
    {
        list(
            $this->id,
            $this->username,
            $this->password
        ) = unserialize($serialized, ['allowed_classes' => false]);
    }
}