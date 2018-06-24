<?php

namespace App\Entity;

use Doctrine\ORM\Mapping as ORM;

/**
 * @ORM\Table(name="invites", schema="users")
 * @ORM\Entity(repositoryClass="App\Repository\InviteRepository")
 */
class Invite
{
    /**
     * @var int
     *
     * @ORM\Id()
     * @ORM\GeneratedValue(strategy="AUTO")
     * @ORM\Column(name="id", type="integer")
     */
    private $id;

    /**
     * @var User
     *
     * @ORM\ManyToOne(targetEntity="App\Entity\User")
     * @ORM\JoinColumn(name="user_id", nullable=false)
     */
    private $user;

    /**
     * @var string
     *
     * @ORM\Column(name="code", type="string", length=32)
     */
    private $code;

    /**
     * @var User|null
     *
     * @ORM\ManyToOne(targetEntity="App\Entity\User")
     * @ORM\JoinColumn(name="used_by_id", nullable=true)
     */
    private $usedBy;

    public function __construct(User $forUser)
    {
        $this->user = $forUser;
        $this->code = md5(random_bytes(100));
    }

    public function getId(): int
    {
        return $this->id;
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function getCode(): string
    {
        return $this->code;
    }

    public function getUsedBy(): ?User
    {
        return $this->usedBy;
    }

    public function use(User $user): void
    {
        if ($this->usedBy) {
            throw new \RuntimeException(sprintf(
                'Invite #%d is already used by User#%d and can\'t be used by User#%d',
                $this->id,
                $this->usedBy->getId(),
                $user->getId()
            ));
        }

        $this->usedBy = $user;
    }
}