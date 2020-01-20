<?php


namespace App\Entity;

use Doctrine\ORM\Mapping as ORM;

/**
 * @ORM\Table(schema="users", name="password_reset_tokens")
 * @ORM\Entity(readOnly=true)
 */
class PasswordResetToken
{
    /**
     * @var User
     *
     * @ORM\ManyToOne(targetEntity="User", fetch="EAGER")
     * @ORM\JoinColumn(name="user_id", nullable=false, onDelete="CASCADE")
     */
    private $user;

    /**
     * @var string
     *
     * @ORM\Id()
     * @ORM\Column(name="code", type="text", nullable=false)
     */
    private $code;

    /**
     * @var \DateTime
     *
     * @ORM\Column(name="valid_until", type="datetime", nullable=false)
     */
    private $validUntil;

    public function __construct(User $user, \DateInterval $validFor = null)
    {
        $this->user = $user;
        $this->code = hash('sha3-384', uniqid('reset', true));

        $now = new \DateTime();

        if ($validFor) {
            $this->validUntil = $now->add($validFor);
        } else {
            $this->validUntil = $now->add(new \DateInterval('P1D'));
        }
    }

    public function isValid(): bool
    {
        $now = new \DateTime();

        return $now < $this->validUntil;
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function getCode(): string
    {
        return $this->code;
    }
}
