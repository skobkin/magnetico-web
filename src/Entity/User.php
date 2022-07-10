<?php
declare(strict_types=1);

namespace App\Entity;

use App\Repository\UserRepository;
use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Component\PasswordHasher\PasswordHasherInterface;
use Symfony\Component\Security\Core\User\{PasswordAuthenticatedUserInterface, UserInterface};

#[ORM\Table(name: 'users', schema: 'users')]
#[ORM\Entity(repositoryClass: UserRepository::class)]
class User implements UserInterface, PasswordAuthenticatedUserInterface
{
    #[ORM\Id]
    #[ORM\Column(name: 'id', type: 'integer')]
    #[ORM\GeneratedValue(strategy: 'AUTO')]
    private int $id;

    #[ORM\Column(name: 'username', type: 'string', length: 25, unique: true)]
    private string $username;

    #[ORM\Column(name: 'password', type: 'text')]
    private string $password;

    #[ORM\Column(name: 'email', type: 'string', length: 254, unique: true)]
    private string $email;

    #[ORM\Column(name: 'roles', type: 'json')]
    private array $roles = [];

    #[ORM\Column(name: 'created_at', type: 'datetime')]
    private \DateTime $createdAt;

    /** @var Invite[]|ArrayCollection */
    #[ORM\OneToMany(targetEntity: Invite::class, mappedBy: 'user', fetch: 'EXTRA_LAZY')]
    private $invites;

    public function __construct(string $username, PasswordHasherInterface $hasher, string $rawPassword, string $email, array $roles = [])
    {
        $this->username = $username;
        $this->password = $hasher->hash($rawPassword);
        $this->email = $email;
        $this->roles = $roles ?: ['ROLE_USER'];
        $this->createdAt = new \DateTime();
    }

    public function getId(): int
    {
        return $this->id;
    }

    public function getUserIdentifier(): string
    {
        return $this->username;
    }

    /** @deprecated since Symfony 5.3, use getUserIdentifier() instead */
    public function getUsername(): string
    {
        return $this->username;
    }

    public function getPassword(): string
    {
        return $this->password;
    }

    public function changePassword(PasswordHasherInterface $hasher, string $rawPassword): void
    {
        $this->password = $hasher->hash($rawPassword);
    }

    /** @deprecated since Symfony 5.3 */
    public function getSalt(): ?string
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
    public function serialize(): string
    {
        return serialize([
            $this->id,
            $this->username,
            $this->password,
        ]);
    }

    /** @see \Serializable::unserialize() */
    public function unserialize($serialized): void
    {
        [
            $this->id,
            $this->username,
            $this->password
        ] = unserialize($serialized, ['allowed_classes' => false]);
    }
}