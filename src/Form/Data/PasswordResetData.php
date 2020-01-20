<?php

namespace App\Form\Data;

use Symfony\Component\Validator\Constraints as Assert;

class PasswordResetData
{
    /**
     * @var string
     *
     * @Assert\NotBlank
     * @Assert\Length(min="8", max="4096")
     * @Assert\NotCompromisedPassword(skipOnError=true)
     */
    public $password;
}
